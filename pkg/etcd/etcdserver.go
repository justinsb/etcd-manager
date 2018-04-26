package etcd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	protoetcd "kope.io/etcd-manager/pkg/apis/etcd"
	"kope.io/etcd-manager/pkg/backup"
	"kope.io/etcd-manager/pkg/contextutil"
	"kope.io/etcd-manager/pkg/privateapi"
)

const PreparedValidity = time.Minute

type EtcdServer struct {
	baseDir               string
	peerServer            *privateapi.Server
	etcdNodeConfiguration *protoetcd.EtcdNode
	clusterName           string

	backupStore backup.Store

	mutex sync.Mutex

	nodeState *protoetcd.NodeState
	prepared  *preparedState
	process   *etcdProcess
}

type preparedState struct {
	validUntil   time.Time
	clusterToken string
}

func NewEtcdServer(baseDir string, clusterName string, etcdNodeConfiguration *protoetcd.EtcdNode, peerServer *privateapi.Server) (*EtcdServer, error) {
	s := &EtcdServer{
		baseDir:               baseDir,
		clusterName:           clusterName,
		peerServer:            peerServer,
		etcdNodeConfiguration: etcdNodeConfiguration,
	}

	// Initialize before starting to serve
	if err := s.readState(); err != nil {
		return nil, err
	}

	protoetcd.RegisterEtcdManagerServiceServer(peerServer.GrpcServer(), s)
	return s, nil
}

var _ protoetcd.EtcdManagerServiceServer = &EtcdServer{}

func (s *EtcdServer) Run(ctx context.Context) {
	contextutil.Forever(ctx, time.Second*10, func() {
		err := s.runOnce()
		if err != nil {
			glog.Warningf("error running etcd loop: %v", err)
		}
	})
}

func readState(baseDir string) (*protoetcd.NodeState, error) {
	p := filepath.Join(baseDir, "state")
	b, err := ioutil.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading state file %q: %v", p, err)
	}

	state := &protoetcd.NodeState{}
	if err := proto.Unmarshal(b, state); err != nil {
		// TODO: Have multiple state files?
		return nil, fmt.Errorf("error parsing state file: %v", err)
	}

	return state, nil
}

// updateState writes state to disk and updates s.nodeState
func (s *EtcdServer) updateState(nodeState *protoetcd.NodeState) error {
	if err := writeStateToDisk(s.baseDir, nodeState); err != nil {
		return err
	}
	// Note that we update the in-memory version _after_ we have successfully written to disk - we "can't" fail at this point
	s.nodeState = nodeState
	return nil
}

func writeStateToDisk(baseDir string, state *protoetcd.NodeState) error {
	p := filepath.Join(baseDir, "state")

	b, err := proto.Marshal(state)
	if err != nil {
		return fmt.Errorf("error marshaling state data: %v", err)
	}

	if err := ioutil.WriteFile(p, b, 0755); err != nil {
		return fmt.Errorf("error writing state file %q: %v", p, err)
	}
	return nil
}

func (s *EtcdServer) readState() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.nodeState == nil {
		nodeState, err := readState(s.baseDir)
		if err != nil {
			return err
		}

		if nodeState != nil {
			s.nodeState = nodeState
		}
	}

	return nil
}

func (s *EtcdServer) runOnce() error {
	if err := s.readState(); err != nil {
		return err
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Check that etcd process is still running
	if s.process != nil {
		exitError, exitState := s.process.ExitState()
		if exitError != nil || exitState != nil {
			glog.Warningf("etc process exited (error=%v, state=%v)", exitError, exitState)

			s.process = nil
		}
	}

	// Start etcd, if it is not running but should be
	if s.nodeState != nil && s.nodeState.EtcdState != nil && s.nodeState.EtcdState.Cluster != nil && s.process == nil {
		if err := s.startEtcdProcess(s.nodeState.EtcdState); err != nil {
			return err
		}
	}

	return nil
}

// GetInfo gets info about the node
func (s *EtcdServer) GetInfo(context.Context, *protoetcd.GetInfoRequest) (*protoetcd.GetInfoResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	response := &protoetcd.GetInfoResponse{}
	response.ClusterName = s.clusterName
	response.NodeConfiguration = s.etcdNodeConfiguration

	if s.nodeState != nil {
		response.EtcdState = s.nodeState.EtcdState
		response.ClusterSpecVersion = s.nodeState.ClusterSpecVersion
	}

	return response, nil
}

// validateHeader checks that the required fields are set and match our cluster
func (s *EtcdServer) validateHeader(header *protoetcd.CommonRequestHeader) error {
	if header.ClusterSpec == nil {
		glog.Infof("request was missing ClusterSpec: %s", header)
		return fmt.Errorf("ClusterSpec missing")
	}

	if header.ClusterName != s.clusterName {
		glog.Infof("request had incorrect ClusterName.  ClusterName=%q but request=%q", s.clusterName, header.ClusterName)
		return fmt.Errorf("ClusterName mismatch")
	}

	if !s.peerServer.IsLeader(header.LeadershipToken) {
		return fmt.Errorf("LeadershipToken in request %q is not current leader", header.LeadershipToken)
	}

	// TODO: Validate (our) peer id?

	return nil
}

// JoinCluster requests that the node join an existing cluster
func (s *EtcdServer) JoinCluster(ctx context.Context, request *protoetcd.JoinClusterRequest) (*protoetcd.JoinClusterResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if err := s.validateHeader(request.Header); err != nil {
		return nil, err
	}

	if s.prepared != nil && time.Now().After(s.prepared.validUntil) {
		glog.Infof("preparation %q expired", s.prepared.clusterToken)
		s.prepared = nil
	}

	_, err := BindirForEtcdVersion(request.EtcdVersion, "etcd")
	if err != nil {
		return nil, fmt.Errorf("etcd version %q not supported", request.EtcdVersion)
	}

	response := &protoetcd.JoinClusterResponse{}

	switch request.Phase {
	case protoetcd.Phase_PHASE_PREPARE:
		if s.process != nil {
			return nil, fmt.Errorf("etcd process already running")
		}

		if s.prepared != nil {
			return nil, fmt.Errorf("concurrent prepare in progress %q", s.prepared.clusterToken)
		}

		s.prepared = &preparedState{
			validUntil:   time.Now().Add(PreparedValidity),
			clusterToken: request.ClusterToken,
		}
		return response, nil

	case protoetcd.Phase_PHASE_INITIAL_CLUSTER:
		if s.process != nil {
			return nil, fmt.Errorf("etcd process already running")
		}

		if s.prepared == nil {
			return nil, fmt.Errorf("not prepared")
		}
		if s.prepared.clusterToken != request.ClusterToken {
			return nil, fmt.Errorf("clusterToken %q does not match prepared %q", request.ClusterToken, s.prepared.clusterToken)
		}

		etcdState := &protoetcd.EtcdState{}

		etcdState.NewCluster = true
		etcdState.Cluster = &protoetcd.EtcdCluster{
			ClusterToken: request.ClusterToken,
			Nodes:        request.Nodes,
		}
		etcdState.Quarantined = true
		etcdState.EtcdVersion = request.EtcdVersion

		nodeState := &protoetcd.NodeState{
			ClusterSpecVersion: request.Header.ClusterSpec,
			EtcdState:          etcdState,
		}

		if err := s.updateState(nodeState); err != nil {
			return nil, err
		}

		if err := s.startEtcdProcess(nodeState.EtcdState); err != nil {
			return nil, err
		}

		// TODO: Wait for etcd initialization before marking as existing?
		nodeState.EtcdState.NewCluster = false
		if err := s.updateState(nodeState); err != nil {
			return nil, err
		}
		s.prepared = nil
		return response, nil

	case protoetcd.Phase_PHASE_JOIN_EXISTING:
		if s.process != nil {
			return nil, fmt.Errorf("etcd process already running")
		}

		if s.prepared == nil {
			return nil, fmt.Errorf("not prepared")
		}
		if s.prepared.clusterToken != request.ClusterToken {
			return nil, fmt.Errorf("clusterToken %q does not match prepared %q", request.ClusterToken, s.prepared.clusterToken)
		}

		etcdState := &protoetcd.EtcdState{}

		etcdState.NewCluster = false
		etcdState.Cluster = &protoetcd.EtcdCluster{
			ClusterToken: request.ClusterToken,
			Nodes:        request.Nodes,
		}
		etcdState.Quarantined = false
		etcdState.EtcdVersion = request.EtcdVersion

		nodeState := &protoetcd.NodeState{
			ClusterSpecVersion: request.Header.ClusterSpec,
			EtcdState:          etcdState,
		}

		if err := s.updateState(nodeState); err != nil {
			return nil, err
		}

		if err := s.startEtcdProcess(nodeState.EtcdState); err != nil {
			return nil, err
		}
		// TODO: Wait for join?
		s.prepared = nil
		return response, nil

	default:
		return nil, fmt.Errorf("unknown status %s", request.Phase)
	}
}

// Reconfigure requests that the node reconfigure itself, in particular for an etcd upgrade/downgrade
func (s *EtcdServer) Reconfigure(ctx context.Context, request *protoetcd.ReconfigureRequest) (*protoetcd.ReconfigureResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	glog.Infof("Reconfigure request: %v", request)

	if err := s.validateHeader(request.Header); err != nil {
		return nil, err
	}

	if s.nodeState == nil || s.nodeState.EtcdState == nil {
		return nil, fmt.Errorf("cluster not running")
	}

	response := &protoetcd.ReconfigureResponse{}

	newState := proto.Clone(s.nodeState).(*protoetcd.NodeState)
	me, err := s.findSelfNode(newState.EtcdState)
	if err != nil {
		return nil, err
	}
	if me == nil {
		return nil, fmt.Errorf("could not find self node in cluster: %v", err)
	}

	//// We just need to restart to update clienturls
	//if len(request.ClientUrls) != 0 {
	//	me.ClientUrls = request.ClientUrls
	//}

	if request.EtcdVersion != "" {
		_, err := BindirForEtcdVersion(request.EtcdVersion, "etcd")
		if err != nil {
			return nil, fmt.Errorf("etcd version %q not supported", request.EtcdVersion)
		}

		newState.EtcdState.EtcdVersion = request.EtcdVersion
	}

	newState.EtcdState.Quarantined = request.Quarantined
	newState.ClusterSpecVersion = request.Header.ClusterSpec

	glog.Infof("Stopping etcd for reconfigure request: %v", request)
	_, err = s.stopEtcdProcess()
	if err != nil {
		return nil, fmt.Errorf("error stoppping etcd process: %v", err)
	}

	if err := s.updateState(newState); err != nil {
		return nil, err
	}

	if err := s.startEtcdProcess(s.nodeState.EtcdState); err != nil {
		return nil, err
	}

	return response, nil
}

// StopEtcd requests the the node stop running etcd
func (s *EtcdServer) StopEtcd(ctx context.Context, request *protoetcd.StopEtcdRequest) (*protoetcd.StopEtcdResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	glog.Infof("StopEtcd request: %v", request)

	if err := s.validateHeader(request.Header); err != nil {
		return nil, err
	}

	if s.nodeState == nil || s.nodeState.EtcdState == nil {
		return nil, fmt.Errorf("cluster not running")
	}

	response := &protoetcd.StopEtcdResponse{}

	glog.Infof("Stopping etcd for stop request: %v", request)
	if _, err := s.stopEtcdProcess(); err != nil {
		return nil, fmt.Errorf("error stopping etcd process: %v", err)
	}

	newState := &protoetcd.NodeState{
		ClusterSpecVersion: request.Header.ClusterSpec,
	}

	if err := s.updateState(newState); err != nil {
		return nil, err
	}

	return response, nil
}

// UpdateState stores the server-provided state on persistent storage
func (s *EtcdServer) UpdateState(ctx context.Context, request *protoetcd.UpdateStateRequest) (*protoetcd.UpdateStateResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	glog.Infof("UpdateState request: %v", request)

	if err := s.validateHeader(request.Header); err != nil {
		return nil, err
	}

	newState := proto.Clone(s.nodeState).(*protoetcd.NodeState)
	newState.ClusterSpecVersion = request.Header.ClusterSpec

	if err := s.updateState(newState); err != nil {
		return nil, err
	}

	response := &protoetcd.UpdateStateResponse{}

	return response, nil
}

// DoBackup performs a backup to the backupstore
func (s *EtcdServer) DoBackup(ctx context.Context, request *protoetcd.DoBackupRequest) (*protoetcd.DoBackupResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if err := s.validateHeader(request.Header); err != nil {
		return nil, err
	}

	if s.process == nil {
		return nil, fmt.Errorf("etcd not running")
	}

	if request.Storage == "" {
		return nil, fmt.Errorf("Storage is required")
	}
	if request.Info == nil {
		return nil, fmt.Errorf("Info is required")
	}

	{
		// TODO: Make this optional?
		newState := proto.Clone(s.nodeState).(*protoetcd.NodeState)
		newState.ClusterSpecVersion = request.Header.ClusterSpec
		if err := s.updateState(newState); err != nil {
			return nil, err
		}
	}

	backupStore, err := backup.NewStore(request.Storage)
	if err != nil {
		return nil, err
	}

	info := request.Info
	info.EtcdVersion = s.process.EtcdVersion

	response, err := s.process.DoBackup(backupStore, info)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (s *EtcdServer) findSelfNode(state *protoetcd.EtcdState) (*protoetcd.EtcdNode, error) {
	var meNode *protoetcd.EtcdNode
	for _, node := range state.Cluster.Nodes {
		if node.Name == s.etcdNodeConfiguration.Name {
			if meNode != nil {
				glog.Infof("Nodes: %v", state.Cluster.Nodes)
				return nil, fmt.Errorf("multiple nodes matching local peer urls %s included in cluster", node.PeerUrls)
			}
			meNode = node
		}
	}
	if meNode == nil {
		glog.Infof("unable to find node in cluster")
		glog.Infof("self node: %v", s.etcdNodeConfiguration)
		glog.Infof("cluster: %v", state.Cluster.Nodes)
	}
	return meNode, nil
}

func (s *EtcdServer) startEtcdProcess(state *protoetcd.EtcdState) error {
	if state.Cluster == nil {
		return fmt.Errorf("cluster not configured, cannot start etcd")
	}
	if state.Cluster.ClusterToken == "" {
		return fmt.Errorf("ClusterToken not configured, cannot start etcd")
	}
	dataDir := filepath.Join(s.baseDir, "data", state.Cluster.ClusterToken)
	glog.Infof("starting etcd %s with datadir %s", state.EtcdVersion, dataDir)

	// TODO: Validate this during the PREPARE phase
	meNode, err := s.findSelfNode(state)
	if err != nil {
		return err
	}
	if meNode == nil {
		return fmt.Errorf("self node was not included in cluster")
	}

	p := &etcdProcess{
		CreateNewCluster: false,
		DataDir:          dataDir,
		Cluster: &protoetcd.EtcdCluster{
			ClusterToken: state.Cluster.ClusterToken,
			Nodes:        state.Cluster.Nodes,
		},
		Quarantined: state.Quarantined,
		MyNodeName:  s.etcdNodeConfiguration.Name,
	}

	binDir, err := BindirForEtcdVersion(state.EtcdVersion, "etcd")
	if err != nil {
		return err
	}
	p.BinDir = binDir
	p.EtcdVersion = state.EtcdVersion

	if state.NewCluster {
		p.CreateNewCluster = true
	}

	if err := p.Start(); err != nil {
		return fmt.Errorf("error starting etcd: %v", err)
	}

	s.process = p

	return nil
}

// StopEtcdProcessForTest terminates etcd if it is running; primarily used for testing
func (s *EtcdServer) StopEtcdProcessForTest() (bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.stopEtcdProcess()
}

// stopEtcdProcess terminates etcd if it is running.  This version assumes the lock is held (unlike StopEtcdProcessForTest)
func (s *EtcdServer) stopEtcdProcess() (bool, error) {
	if s.process == nil {
		return false, nil
	}

	glog.Infof("killing etcd with datadir %s", s.process.DataDir)
	err := s.process.Stop()
	if err != nil {
		return true, fmt.Errorf("error killing etcd: %v", err)
	}
	s.process = nil
	return true, nil
}
