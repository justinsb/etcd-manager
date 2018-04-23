/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package volumes

import (
	"time"

	"github.com/golang/glog"
)

var (
	// Containerized indicates the etcd is containerized
	Containerized = false
	// // RootFS is the root fs path
	// RootFS = "/"
)

// Boot is the options for the protokube service
type Boot struct {
	// // Channels is a list of channel to apply
	// Channels []string
	// // InitializeRBAC should be set to true if we should create the core RBAC roles
	// InitializeRBAC bool
	// // InternalDNSSuffix is the dns zone we are living in
	// InternalDNSSuffix string
	// // InternalIP is the internal ip address of the node
	// InternalIP net.IP
	// // ApplyTaints controls whether we set taints based on the master label
	// ApplyTaints bool
	// // // DNS is the dns provider
	// // DNS DNSProvider
	// // ModelDir is the model directory
	// ModelDir string
	// // EtcdBackupImage is the image to use for backing up etcd
	// EtcdBackupImage string
	// // EtcdBackupStore is the VFS path to which we should backup etcd
	// EtcdBackupStore string
	// // Etcd container registry location.
	// EtcdImageSource string
	// // EtcdElectionTimeout is is the leader election timeout
	// EtcdElectionTimeout string
	// // EtcdHeartbeatInterval is the heartbeat interval
	// EtcdHeartbeatInterval string
	// // TLSAuth indicates we should enforce peer and client verification
	// TLSAuth bool
	// // TLSCA is the path to a client ca for etcd
	// TLSCA string
	// // TLSCert is the path to a tls certificate for etcd
	// TLSCert string
	// // TLSKey is the path to a tls private key for etcd
	// TLSKey string
	// // PeerCA is the path to a peer ca for etcd
	// PeerCA string
	// // PeerCert is the path to a peer certificate for etcd
	// PeerCert string
	// // PeerKey is the path to a peer private key for etcd
	// PeerKey string
	// // // Kubernetes is the context methods for kubernetes
	// // Kubernetes *KubernetesContext
	// // Master indicates we are a master node
	// Master bool

	volumeMounter *VolumeMountController
	// etcdControllers map[string]*EtcdController
}

// Init is responsible for initializing the controllers
func (b *Boot) Init(volumesProvider Volumes) {
	b.volumeMounter = newVolumeMountController(volumesProvider)
}

// WaitForVolumes
func (b *Boot) WaitForVolumes() []*Volume {
	for {
		info, err := b.tryMountVolumes()
		if err != nil {
			glog.Warningf("error during attempt to bootstrap (will sleep and retry): %v", err)
			continue
		} else if len(info) != 0 {
			return info
		} else {
			glog.Infof("waiting for volumes")
		}

		time.Sleep(1 * time.Minute)
	}
}

func (b *Boot) tryMountVolumes() ([]*Volume, error) {
	// attempt to mount the volumes
	volumes, err := b.volumeMounter.mountMasterVolumes()
	if err != nil {
		return nil, err
	}

	return volumes, nil
}

func PathFor(hostPath string) string {
	if hostPath[0] != '/' {
		glog.Fatalf("path was not absolute: %q", hostPath)
	}
	rootfs := "/"
	if Containerized {
		rootfs = "/rootfs/"
	}
	return rootfs + hostPath[1:]
}
