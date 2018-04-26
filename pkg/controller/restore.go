package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	protoetcd "kope.io/etcd-manager/pkg/apis/etcd"
)

func (m *EtcdController) restoreBackupAndLiftQuarantine(parentContext context.Context, clusterState *etcdClusterState, backupName string) (bool, error) {
	changed := false

	// We start a new context - this is pretty critical-path
	ctx := context.Background()

	// var restoreRequest *protoetcd.DoRestoreRequest
	// {
	// 	backups, err := m.backupStore.ListBackups()
	// 	if err != nil {
	// 		return false, fmt.Errorf("error listing backups: %v", err)
	// 	}

	// 	for i := len(backups) - 1; i >= 0; i-- {
	// 		backup := backups[i]

	// 		info, err := m.backupStore.LoadInfo(backup)
	// 		if err != nil {
	// 			glog.Warningf("error reading backup info in %q: %v", backup, err)
	// 			continue
	// 		}
	// 		if info == nil || info.EtcdVersion == "" {
	// 			glog.Warningf("etcd version not found in %q", backup)
	// 			continue
	// 		} else {
	// 			restoreRequest = &protoetcd.DoRestoreRequest{
	// 				Header:     m.buildHeader(),
	// 				Storage:    m.backupStore.Spec(),
	// 				BackupName: backup,
	// 			}
	// 			break
	// 		}
	// 	}
	// }

	if backupName != "" {
		info, err := m.backupStore.LoadInfo(backupName)
		if err != nil {
			return false, fmt.Errorf("error reading backup info in %q: %v", backupName, err)
		}

		if info == nil || info.EtcdVersion == "" {
			return false, fmt.Errorf("etcd version not found in %q", backupName)
		}

		restoreRequest := &protoetcd.DoRestoreRequest{
			Header:     m.buildHeader(),
			Storage:    m.backupStore.Spec(),
			BackupName: backupName,
		}

		if restoreRequest != nil {
			var peer *etcdClusterPeerInfo
			for peerId, p := range clusterState.peers {
				member := clusterState.FindHealthyMember(peerId)
				if member == nil {
					continue
				}
				peer = p
			}
			if peer == nil {
				return false, fmt.Errorf("unable to find peer on which to run restore operation")
			}

			response, err := peer.peer.rpcDoRestore(ctx, restoreRequest)
			if err != nil {
				return false, fmt.Errorf("error restoring backup on peer %v: %v", peer.peer, err)
			} else {
				changed = true
			}
			glog.V(2).Infof("DoRestoreResponse: %s", response)
		}
	}

	// Update cluster spec
	{
		newClusterSpec := proto.Clone(m.clusterSpecVersion).(*protoetcd.ClusterSpecVersion)
		// Don't restore backup again
		newClusterSpec.ClusterSpec.RestoreBackup = ""
		newClusterSpec.Timestamp = time.Now().UnixNano()

		err := m.writeClusterSpec(ctx, clusterState, newClusterSpec)
		if err != nil {
			return false, err
		}
	}

	updated, err := m.updateQuarantine(ctx, clusterState, false)
	if err != nil {
		return changed, err
	}
	if updated {
		changed = true
	}
	return changed, nil
}

func (m *EtcdController) updateQuarantine(ctx context.Context, clusterState *etcdClusterState, quarantined bool) (bool, error) {
	glog.Infof("Setting quarantined state to %t", quarantined)

	// Sanity check that EtcdState is non-nil, so we can set quarantined in the second loop
	for peerId, p := range clusterState.peers {
		member := clusterState.FindMember(peerId)
		if member == nil {
			continue
		}

		if p.info.EtcdState == nil {
			return false, fmt.Errorf("peer was member but did not have EtcdState")
		}
		if p.info.NodeConfiguration == nil {
			return false, fmt.Errorf("peer was member but did not have NodeConfiguration")
		}
	}

	changed := false
	for peerId, p := range clusterState.peers {
		member := clusterState.FindMember(peerId)
		if member == nil {
			continue
		}

		request := &protoetcd.ReconfigureRequest{
			Header:      m.buildHeader(),
			Quarantined: quarantined,
		}

		response, err := p.peer.rpcReconfigure(ctx, request)
		if err != nil {
			return changed, fmt.Errorf("error reconfiguring peer %v to not be quarantined: %v", p.peer, err)
		}
		changed = true
		glog.V(2).Infof("ReconfigureResponse: %s", response)

		p.info.EtcdState.Quarantined = quarantined
		clientUrls := p.info.NodeConfiguration.ClientUrls
		if p.info.EtcdState.Quarantined {
			clientUrls = p.info.NodeConfiguration.QuarantinedClientUrls
		}
		member.ClientURLs = clientUrls
	}
	return changed, nil
}
