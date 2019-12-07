/*
Copyright 2017 The Kubernetes Authors.

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
	"github.com/golang/glog"
	"k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

// Constants from nsenter_mount.go
const (
	hostMountNamespacePath = "/rootfs/proc/1/ns/mnt"
	nsenterPath            = "nsenter"
)

// NewNsEnterExec builds a mount.Exec implementation that nsenters into the host process
// It is very similar to mount.NewNsenterMounter, but execs into the host
func NewNsEnterExec() mount.Exec {
	return &nsEnterExec{}
}

// nsEnterExec is an implementation of mount.Exec that runs in the host namespace
type nsEnterExec struct{}

var _ mount.Exec = &nsEnterExec{}

// Run implements mount.Exec::Run but runs proceses in the host namespace
func (e *nsEnterExec) Run(cmd string, args ...string) ([]byte, error) {
	nsenterArgs := []string{
		"--mount=" + hostMountNamespacePath,
		"--",
		cmd,
	}
	nsenterArgs = append(nsenterArgs, args...)
	glog.V(5).Infof("Running command : %v %v", nsenterPath, nsenterArgs)
	exe := exec.New()
	return exe.Command(nsenterPath, nsenterArgs...).CombinedOutput()
}
