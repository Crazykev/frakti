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

package manager

import (
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/frakti/pkg/runtime"
	kubeapi "k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	utilexec "k8s.io/kubernetes/pkg/util/exec"
)

const (
	runtimeAPIVersion = "0.1.0"

	// TODO(resouer) move this to well-known labels on k8s upstream?
	// The annotation key specifying this pod will run by OS container runtime.
	OSContainerAnnotationKey = "runtime.frakti.alpha.kubernetes.io/OSContainer"
	// The annotation value specifying this pod will run by OS container runtime.
	OSContainerAnnotationTrue = "true"

	// runtime service names for error log
	hyperRuntimeServiceName       = "hyper runtime service"
	alternativeRuntimeServiceName = "alternative runtime service"
)

// FraktiManager serves the kubelet runtime gRPC api which will be
// consumed by kubelet
type FraktiManager struct {
	// The grpc server.
	server *grpc.Server
	// The streaming server.
	streamingServer streaming.Server

	runtimeService runtime.RuntimeService
	imageService   runtime.ImageService

	alternativeRuntimeService dockershim.DockerService
	alternativeImageService   dockershim.DockerService

	// The pod sets need to be processed by alternative runtime
	cachedAlternativeRuntimeItems *alternativeRuntimeSets
}

// NewFraktiManager creates a new FraktiManager
func NewFraktiManager(
	runtimeService runtime.RuntimeService,
	imageService runtime.ImageService,
	streamingServer streaming.Server,
	alternativeRuntimeService dockershim.DockerService,
	alternativeImageService dockershim.DockerService,
) (*FraktiManager, error) {
	s := &FraktiManager{
		server:                        grpc.NewServer(),
		runtimeService:                runtimeService,
		imageService:                  imageService,
		streamingServer:               streamingServer,
		alternativeRuntimeService:     alternativeRuntimeService,
		alternativeImageService:       alternativeImageService,
		cachedAlternativeRuntimeItems: newAlternativeRuntimeSets(),
	}
	if alternativeRuntimeService != nil {
		sandboxes, err := s.alternativeRuntimeService.ListPodSandbox(nil)
		if err != nil {
			glog.Errorf("Failed to initialize frakti manager: ListPodSandbox from alternative runtime service failed: %v", err)
			return nil, err
		}
		containers, err := s.alternativeRuntimeService.ListContainers(nil)
		if err != nil {
			glog.Errorf("Failed to initialize frakti manager: ListContainers from alternative runtime service failed: %v", err)
			return nil, err
		}
		for _, sandbox := range sandboxes {
			s.cachedAlternativeRuntimeItems.add(sandbox.Id)
		}
		for _, container := range containers {
			s.cachedAlternativeRuntimeItems.add(container.Id)
		}
		glog.Infof("Restored alternative runtime managed sandboxes and containers to cache")
	}
	s.registerServer()

	return s, nil
}

// Serve starts gRPC server at unix://addr
func (s *FraktiManager) Serve(addr string) error {
	glog.V(1).Infof("Start frakti at %s", addr)

	if err := syscall.Unlink(addr); err != nil && !os.IsNotExist(err) {
		return err
	}

	if s.streamingServer != nil {
		go func() {
			if err := s.streamingServer.Start(true); err != nil {
				glog.Fatalf("Failed to start streaming server: %v", err)
			}
		}()
	}

	lis, err := net.Listen("unix", addr)
	if err != nil {
		glog.Fatalf("Failed to listen %s: %v", addr, err)
		return err
	}

	defer lis.Close()
	return s.server.Serve(lis)
}

func (s *FraktiManager) registerServer() {
	kubeapi.RegisterRuntimeServiceServer(s.server, s)
	kubeapi.RegisterImageServiceServer(s.server, s)
}

// Version returns the runtime name, runtime version and runtime API version.
func (s *FraktiManager) Version(ctx context.Context, req *kubeapi.VersionRequest) (*kubeapi.VersionResponse, error) {
	// Version API use hyper runtime service
	resp, err := s.runtimeService.Version(runtimeAPIVersion)
	if err != nil {
		glog.Errorf("Get version from runtime service failed: %v", err)
		return nil, err
	}

	return resp, nil
}

// RunPodSandbox creates and start a hyper Pod.
func (s *FraktiManager) RunPodSandbox(ctx context.Context, req *kubeapi.RunPodSandboxRequest) (*kubeapi.RunPodSandboxResponse, error) {
	glog.V(3).Infof("RunPodSandbox from runtime service with request %s", req.String())
	var (
		podID       string
		err         error
		runtimeName string
	)
	// TODO(harry) use a runtime wrapper to remove all runtimeName here and below
	if isOSContainerRuntimeRequired(req.GetConfig()) {
		runtimeName = alternativeRuntimeServiceName
		podID, err = s.alternativeRuntimeService.RunPodSandbox(req.Config)
	} else {
		runtimeName = hyperRuntimeServiceName
		podID, err = s.runtimeService.RunPodSandbox(req.Config)
	}
	if err != nil {
		glog.Errorf("RunPodSandbox from %s failed: %v", runtimeName, err)
		return nil, err
	}
	if isOSContainerRuntimeRequired(req.GetConfig()) {
		s.cachedAlternativeRuntimeItems.add(podID)
	}
	return &kubeapi.RunPodSandboxResponse{PodSandboxId: podID}, nil
}

// StopPodSandbox stops the sandbox.
func (s *FraktiManager) StopPodSandbox(ctx context.Context, req *kubeapi.StopPodSandboxRequest) (*kubeapi.StopPodSandboxResponse, error) {
	glog.V(3).Infof("StopPodSandbox from runtime service with request %s", req.String())
	var (
		runtimeName string
		err         error
	)
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		runtimeName = alternativeRuntimeServiceName
		err = s.alternativeRuntimeService.StopPodSandbox(req.PodSandboxId)
	} else {
		runtimeName = hyperRuntimeServiceName
		err = s.runtimeService.StopPodSandbox(req.PodSandboxId)
	}
	if err != nil {
		glog.Errorf("StopPodSandbox from %s failed: %v", runtimeName, err)
		return nil, err
	}

	return &kubeapi.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox deletes the sandbox.
func (s *FraktiManager) RemovePodSandbox(ctx context.Context, req *kubeapi.RemovePodSandboxRequest) (*kubeapi.RemovePodSandboxResponse, error) {
	glog.V(3).Infof("RemovePodSandbox from runtime service with request %s", req.String())
	var (
		runtimeName string
		err         error
	)
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		runtimeName = alternativeRuntimeServiceName
		err = s.alternativeRuntimeService.RemovePodSandbox(req.PodSandboxId)
	} else {
		runtimeName = hyperRuntimeServiceName
		err = s.runtimeService.RemovePodSandbox(req.PodSandboxId)
	}
	if err != nil {
		glog.Errorf("RemovePodSandbox from %s failed: %v", runtimeName, err)
		return nil, err
	}
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		s.cachedAlternativeRuntimeItems.remove(req.PodSandboxId)
	}
	return &kubeapi.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the Status of the PodSandbox.
func (s *FraktiManager) PodSandboxStatus(ctx context.Context, req *kubeapi.PodSandboxStatusRequest) (*kubeapi.PodSandboxStatusResponse, error) {
	glog.V(3).Infof("PodSandboxStatus with request %s", req.String())
	var (
		podStatus   *kubeapi.PodSandboxStatus
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		runtimeName = alternativeRuntimeServiceName
		podStatus, err = s.alternativeRuntimeService.PodSandboxStatus(req.PodSandboxId)
	} else {
		runtimeName = hyperRuntimeServiceName
		podStatus, err = s.runtimeService.PodSandboxStatus(req.PodSandboxId)
	}
	if err != nil {
		glog.Errorf("PodSandboxStatus from %s failed: %v", runtimeName, err)
		return nil, err
	}
	return &kubeapi.PodSandboxStatusResponse{Status: podStatus}, nil
}

// ListPodSandbox returns a list of SandBox.
func (s *FraktiManager) ListPodSandbox(ctx context.Context, req *kubeapi.ListPodSandboxRequest) (*kubeapi.ListPodSandboxResponse, error) {
	glog.V(3).Infof("ListPodSandbox with request %s", req.String())

	items, err := s.runtimeService.ListPodSandbox(req.GetFilter())
	if err != nil {
		glog.Errorf("ListPodSandbox from runtime service failed: %v", err)
		return nil, err
	}

	if s.alternativeRuntimeService != nil {
		podsInOsContainerRuntime, err := s.alternativeRuntimeService.ListPodSandbox(req.GetFilter())
		if err != nil {
			glog.Errorf("ListPodSandbox from alternative runtime service failed: %v", err)
			return nil, err
		}
		items = append(items, podsInOsContainerRuntime...)
	}

	return &kubeapi.ListPodSandboxResponse{Items: items}, nil
}

// CreateContainer creates a new container in specified PodSandbox
func (s *FraktiManager) CreateContainer(ctx context.Context, req *kubeapi.CreateContainerRequest) (*kubeapi.CreateContainerResponse, error) {
	glog.V(3).Infof("CreateContainer with request %s", req.String())
	var (
		containerID string
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		runtimeName = alternativeRuntimeServiceName
		containerID, err = s.alternativeRuntimeService.CreateContainer(req.PodSandboxId, req.Config, req.SandboxConfig)
	} else {
		runtimeName = hyperRuntimeServiceName
		containerID, err = s.runtimeService.CreateContainer(req.PodSandboxId, req.Config, req.SandboxConfig)
	}

	if err != nil {
		glog.Errorf("CreateContainer from %s failed: %v", runtimeName, err)
		return nil, err
	}
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		s.cachedAlternativeRuntimeItems.add(containerID)
		glog.V(3).Infof("added container: %s to alternative runtime container sets", containerID)
	}
	return &kubeapi.CreateContainerResponse{ContainerId: containerID}, nil
}

// StartContainer starts the container.
func (s *FraktiManager) StartContainer(ctx context.Context, req *kubeapi.StartContainerRequest) (*kubeapi.StartContainerResponse, error) {
	glog.V(3).Infof("StartContainer with request %s", req.String())

	var (
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		err = s.alternativeRuntimeService.StartContainer(req.ContainerId)
	} else {
		runtimeName = hyperRuntimeServiceName
		err = s.runtimeService.StartContainer(req.ContainerId)
	}
	if err != nil {
		glog.Errorf("StartContainer from %s failed: %v", runtimeName, err)
		return nil, err
	}
	return &kubeapi.StartContainerResponse{}, nil
}

// StopContainer stops a running container with a grace period (i.e. timeout).
func (s *FraktiManager) StopContainer(ctx context.Context, req *kubeapi.StopContainerRequest) (*kubeapi.StopContainerResponse, error) {
	glog.V(3).Infof("StopContainer with request %s", req.String())

	var (
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		err = s.alternativeRuntimeService.StopContainer(req.ContainerId, req.Timeout)
	} else {
		runtimeName = hyperRuntimeServiceName
		err = s.runtimeService.StopContainer(req.ContainerId, req.Timeout)
	}
	if err != nil {
		glog.Errorf("StopContainer from %s failed: %v", runtimeName, err)
		return nil, err
	}
	return &kubeapi.StopContainerResponse{}, nil
}

// RemoveContainer removes the container.
func (s *FraktiManager) RemoveContainer(ctx context.Context, req *kubeapi.RemoveContainerRequest) (*kubeapi.RemoveContainerResponse, error) {
	glog.V(3).Infof("RemoveContainer with request %s", req.String())

	var (
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		err = s.alternativeRuntimeService.RemoveContainer(req.ContainerId)
	} else {
		runtimeName = hyperRuntimeServiceName
		err = s.runtimeService.RemoveContainer(req.ContainerId)
	}
	if err != nil {
		glog.Errorf("RemoveContainer from %s failed: %v", runtimeName, err)
		return nil, err
	}
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		s.cachedAlternativeRuntimeItems.remove(req.ContainerId)
		glog.V(3).Infof("removed container: %s from alternative runtime container sets", req.ContainerId)
	}
	return &kubeapi.RemoveContainerResponse{}, nil
}

// ListContainers lists all containers by filters.
func (s *FraktiManager) ListContainers(ctx context.Context, req *kubeapi.ListContainersRequest) (*kubeapi.ListContainersResponse, error) {
	glog.V(3).Infof("ListContainers with request %s", req.String())
	var (
		containers []*kubeapi.Container
		err        error
	)
	// kubelet always ant to list all containers, it use filter to get what it want
	vmContainers, err := s.runtimeService.ListContainers(req.GetFilter())
	if err != nil {
		glog.Errorf("ListContainers from hyper runtime service failed: %v", err)
		return nil, err
	}
	containers = append(containers, vmContainers...)

	if s.alternativeRuntimeService != nil {
		osContainers, err := s.alternativeRuntimeService.ListContainers(req.GetFilter())
		if err != nil {
			glog.Errorf("ListContainers from alternative runtime service failed: %v", err)
			return nil, err
		}
		containers = append(containers, osContainers...)
	}

	return &kubeapi.ListContainersResponse{
		Containers: containers,
	}, nil
}

// ContainerStatus returns the container status.
func (s *FraktiManager) ContainerStatus(ctx context.Context, req *kubeapi.ContainerStatusRequest) (*kubeapi.ContainerStatusResponse, error) {
	glog.V(3).Infof("ContainerStatus with request %s", req.String())
	var (
		kubeStatus  *kubeapi.ContainerStatus
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		kubeStatus, err = s.alternativeRuntimeService.ContainerStatus(req.ContainerId)
	} else {
		runtimeName = hyperRuntimeServiceName
		kubeStatus, err = s.runtimeService.ContainerStatus(req.ContainerId)
	}
	if err != nil {
		glog.Errorf("ContainerStatus from %s failed: %v", runtimeName, err)
		return nil, err
	}

	return &kubeapi.ContainerStatusResponse{
		Status: kubeStatus,
	}, nil
}

// ExecSync runs a command in a container synchronously.
func (s *FraktiManager) ExecSync(ctx context.Context, req *kubeapi.ExecSyncRequest) (*kubeapi.ExecSyncResponse, error) {
	glog.V(3).Infof("ExecSync with request %s", req.String())
	var (
		stdout, stderr []byte
		err            error
		runtimeName    string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		stdout, stderr, err = s.alternativeRuntimeService.ExecSync(req.ContainerId, req.Cmd, time.Duration(req.Timeout)*time.Second)
	} else {
		runtimeName = hyperRuntimeServiceName
		stdout, stderr, err = s.runtimeService.ExecSync(req.ContainerId, req.Cmd, time.Duration(req.Timeout)*time.Second)
	}
	var exitCode int32
	if err != nil {
		exitError, ok := err.(utilexec.ExitError)
		if !ok {
			glog.Errorf("ExecSync from %s failed: %v", runtimeName, err)
			return nil, err
		}
		exitCode = int32(exitError.ExitStatus())
	}

	return &kubeapi.ExecSyncResponse{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
	}, nil
}

// Exec prepares a streaming endpoint to execute a command in the container.
func (s *FraktiManager) Exec(ctx context.Context, req *kubeapi.ExecRequest) (*kubeapi.ExecResponse, error) {
	glog.V(3).Infof("Exec with request %s", req.String())
	var (
		resp        *kubeapi.ExecResponse
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		resp, err = s.alternativeRuntimeService.Exec(req)
	} else {
		runtimeName = hyperRuntimeServiceName
		resp, err = s.runtimeService.Exec(req)
	}

	if err != nil {
		glog.Errorf("Exec from %s failed: %v", runtimeName, err)
		return nil, err
	}

	return resp, nil
}

// Attach prepares a streaming endpoint to attach to a running container.
func (s *FraktiManager) Attach(ctx context.Context, req *kubeapi.AttachRequest) (*kubeapi.AttachResponse, error) {
	glog.V(3).Infof("Attach with request %s", req.String())
	var (
		resp        *kubeapi.AttachResponse
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.ContainerId) {
		runtimeName = alternativeRuntimeServiceName
		resp, err = s.alternativeRuntimeService.Attach(req)
	} else {
		runtimeName = hyperRuntimeServiceName
		resp, err = s.runtimeService.Attach(req)
	}
	if err != nil {
		glog.Errorf("Attach from %s failed: %v", runtimeName, err)
		return nil, err
	}

	return resp, nil
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox.
func (s *FraktiManager) PortForward(ctx context.Context, req *kubeapi.PortForwardRequest) (*kubeapi.PortForwardResponse, error) {
	glog.V(3).Infof("PortForward with request %s", req.String())
	var (
		resp        *kubeapi.PortForwardResponse
		err         error
		runtimeName string
	)
	if s.cachedAlternativeRuntimeItems.has(req.PodSandboxId) {
		runtimeName = alternativeRuntimeServiceName
		resp, err = s.alternativeRuntimeService.PortForward(req)

	} else {
		runtimeName = hyperRuntimeServiceName
		resp, err = s.runtimeService.PortForward(req)
	}
	if err != nil {
		glog.Errorf("PortForward from %s failed: %v", runtimeName, err)
		return nil, err
	}
	return resp, nil
}

// UpdateRuntimeConfig updates runtime configuration if specified
func (s *FraktiManager) UpdateRuntimeConfig(ctx context.Context, req *kubeapi.UpdateRuntimeConfigRequest) (*kubeapi.UpdateRuntimeConfigResponse, error) {
	glog.V(3).Infof("Update hyper runtime configure with request %s", req.String())
	// TODO(resouer) only for hyper runtime update, so we cannot deal with handles podCIDR updates in docker.
	err := s.runtimeService.UpdateRuntimeConfig(req.GetRuntimeConfig())
	if err != nil {
		return nil, err
	}
	return &kubeapi.UpdateRuntimeConfigResponse{}, nil
}

// Status returns the status of the runtime.
func (s *FraktiManager) Status(ctx context.Context, req *kubeapi.StatusRequest) (*kubeapi.StatusResponse, error) {
	glog.V(3).Infof("Status hyper runtime service with request %s", req.String())
	var (
		resp *kubeapi.RuntimeStatus
		err  error
	)
	resp, err = s.runtimeService.Status()
	if err != nil {
		return nil, err
	}

	if s.alternativeRuntimeService != nil {
		alternativeResp, err := s.alternativeRuntimeService.Status()
		if err != nil {
			return nil, fmt.Errorf("Status request succeed for hyper, but failed for alternative runtime: %v", err)
		}
		glog.V(3).Infof("Status of alternative runtime service is %v", alternativeResp)
	}

	return &kubeapi.StatusResponse{
		Status: resp,
	}, nil
}

// ListImages lists existing images.
func (s *FraktiManager) ListImages(ctx context.Context, req *kubeapi.ListImagesRequest) (*kubeapi.ListImagesResponse, error) {
	glog.V(3).Infof("ListImages with request %s", req.String())

	// NOTE: we only show image list of hyper runtime and assume alternative runtime images are the same
	images, err := s.imageService.ListImages(req.GetFilter())
	if err != nil {
		glog.Errorf("ListImages from hyper image service failed: %v", err)
		return nil, err
	}

	return &kubeapi.ListImagesResponse{
		Images: images,
	}, nil
}

// ImageStatus returns the status of the image.
func (s *FraktiManager) ImageStatus(ctx context.Context, req *kubeapi.ImageStatusRequest) (*kubeapi.ImageStatusResponse, error) {
	glog.V(3).Infof("ImageStatus with request %s", req.String())

	// NOTE: we only show image status of hyper runtime and assume alternative runtime image are the same
	status, err := s.imageService.ImageStatus(req.Image)
	if err != nil {
		glog.Errorf("ImageStatus from hyper image service failed: %v", err)
		return nil, err
	}
	return &kubeapi.ImageStatusResponse{Image: status}, nil
}

// PullImage pulls a image with authentication config.
func (s *FraktiManager) PullImage(ctx context.Context, req *kubeapi.PullImageRequest) (*kubeapi.PullImageResponse, error) {
	glog.V(3).Infof("PullImage with request %s", req.String())

	var lock sync.Mutex
	var firstImageRef string
	var firstError error
	catchImageRef := func(image string) {
		lock.Lock()
		defer lock.Unlock()
		if firstImageRef == "" {
			firstImageRef = image
		}
	}
	catchError := func(err error) {
		lock.Lock()
		defer lock.Unlock()
		if firstError == nil {
			firstError = err
		}
	}
	pullImageFunc := func(i int) {
		var (
			imageRef string
			err      error
		)
		if i == 0 {
			imageRef, err = s.imageService.PullImage(req.Image, req.Auth)
			if err != nil {
				catchError(fmt.Errorf("PullImage from hyper image service failed: %v", err))
			}
		} else {
			imageRef, err = s.alternativeImageService.PullImage(req.Image, req.Auth)
			if err != nil {
				catchError(fmt.Errorf("PullImage from alternative image service failed: %v", err))
			}
		}

		catchImageRef(imageRef)
	}

	workqueue.Parallelize(2, 2, pullImageFunc)

	if firstError != nil {
		return nil, firstError
	} else {
		return &kubeapi.PullImageResponse{
			ImageRef: firstImageRef,
		}, nil
	}
}

// RemoveImage removes the image.
func (s *FraktiManager) RemoveImage(ctx context.Context, req *kubeapi.RemoveImageRequest) (*kubeapi.RemoveImageResponse, error) {
	glog.V(3).Infof("RemoveImage with request %s", req.String())

	// NOTE: we use hyper images as the view of image list, so do it first and return error immediately when it fails
	err := s.imageService.RemoveImage(req.Image)
	if err != nil {
		glog.Errorf("RemoveImage from hyper image service failed: %v", err)
		return nil, err
	} else {
		err = s.alternativeImageService.RemoveImage(req.Image)
		if err != nil {
			glog.Errorf("RemoveImage hyper image succeed but from alternative image service failed: %v", err)
			return nil, err
		}
	}
	return &kubeapi.RemoveImageResponse{}, nil
}

// isOSContainerRuntimeRequired check if this pod requires to run with os container runtime.
func isOSContainerRuntimeRequired(podConfig *kubeapi.PodSandboxConfig) bool {
	// user require it
	if annotations := podConfig.GetAnnotations(); annotations != nil {
		if useOSContainer := annotations[OSContainerAnnotationKey]; useOSContainer == OSContainerAnnotationTrue {
			return true
		}
	}
	// use host namespace
	if nsOptions := podConfig.GetLinux().GetSecurityContext().GetNamespaceOptions(); nsOptions != nil {
		if nsOptions.HostIpc || nsOptions.HostNetwork || nsOptions.HostPid {
			return true
		}
	}
	return false
}
