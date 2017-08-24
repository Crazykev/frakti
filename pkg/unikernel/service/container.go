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

package service

import (
	"fmt"
	"github.com/docker/docker/pkg/stringid"
	"k8s.io/frakti/pkg/unikernel/metadata"

	kubeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
)

// CreateContainer creates a new container in specified PodSandbox
func (u *UnikernelRuntime) CreateContainer(podSandboxID string, config *kubeapi.ContainerConfig, sandboxConfig *kubeapi.PodSandboxConfig) (string, error) {
	var err error
	name := config.GetMetadata().Name
	// Check if there is any pod already created in pod
	// We now only support one-container-per-pod
	createdContainers := gerAllContainersInPod(podSandboxID)
	for _, container := range createdContainers {
		glog.Warningf("Unikernel/CreateContainer: container(%q) already exist in pod(%q), remove it", container.GetID(), podSandboxID)
		if err := u.RemoveContainer(container.GetID()); err != nil {
			glog.Errorf("Clean up legacy container(%q) failed: %v", container.GetID(), err)
		}
	}

	sandbox, err := u.sandboxStore.Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox(%q) from store: %v", podSandboxID, err)
	}

	cid := generateID()
	cName := makeContainerName(config.GetMetadata(), sandboxConfig.GetMetadata())
	if err = u.containerNameIndex.Reserve(cName, cid); err != nil {
		return nil, fmt.Errorf("reserve container name %q failed: %v", cName, err)
	}
	defer func() {
		if err != nil {
			u.containerNameIndex.ReleaseByName(cName)
		}
	}()

	// Create internal container metadata
	meta := metadata.ContainerMetadata{
		ID:        cid,
		Name:      cName,
		SandboxID: podSandboxID,
		Config:    config,
	}

	// TODO(Crazykev): Prepare container image

	// Create container in VM, for now we actually create VM
	u.vmTools.CreateContainer()

	return "", fmt.Errorf("not implemented")
}

// StartContainer starts the container.
func (u *UnikernelRuntime) StartContainer(rawContainerID string) error {
	return fmt.Errorf("not implemented")
}

// StopContainer stops a running container with a grace period (i.e. timeout).
func (u *UnikernelRuntime) StopContainer(rawContainerID string, timeout int64) error {
	return fmt.Errorf("not implemented")
}

// RemoveContainer removes the container. If the container is running, the container
// should be force removed.
func (u *UnikernelRuntime) RemoveContainer(rawContainerID string) error {
	return fmt.Errorf("not implemented")
}

// ListContainers lists all containers by filters.
func (u *UnikernelRuntime) ListContainers(filter *kubeapi.ContainerFilter) ([]*kubeapi.Container, error) {
	return nil, fmt.Errorf("not implemented")
}

// ContainerStatus returns the container status.
func (u *UnikernelRuntime) ContainerStatus(containerID string) (*kubeapi.ContainerStatus, error) {
	return nil, fmt.Errorf("not implemented")
}

func (u *UnikernelRuntime) getAllContainersInPod(podID string) []*metadata.ContainerMetadata {
	containersInStore := u.containerStore.List()
	var containers []*metadata.ContainerMetadata
	for _, container := range containersInStore {
		if container.SandboxID == podID {
			containers = append(containers, container)
		}
	}
	return containers
}

// generateID generates a random unique id.
func generateID() string {
	return stringid.GenerateNonCryptoID()
}

func makeContainerName(c *kubeapi.ContainerMetadata, s *kubeapi.PodSandboxMetadata) string {
	return strings.Join([]string{
		c.Name,
		s.Name,
		s.Namespace,
		s.Uid,
		fmt.Sprintf("%d", c.Attempt),
	}, "_")
}
