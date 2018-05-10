package openstack

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/gophercloud/gophercloud/openstack/container/v1/capsules"
	"github.com/virtual-kubelet/virtual-kubelet/manager"
	"github.com/virtual-kubelet/virtual-kubelet/providers"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ZunProvider implements the virtual-kubelet provider interface and communicates with OpenStack's Zun APIs.
type ZunProvider struct {
	ZunClient          *gophercloud.ServiceClient
	resourceManager    *manager.ResourceManager
	region             string
	nodeName           string
	operatingSystem    string
	cpu                string
	memory             string
	pods               string
	daemonEndpointPort int32
}

// NewZunProvider creates a new ZunProvider.
func NewZunProvider(config string, rm *manager.ResourceManager, nodeName, operatingSystem string, daemonEndpointPort int32) (*ZunProvider, error) {
	var p ZunProvider
	var err error

	p.resourceManager = rm

	AuthOptions, err := openstack.AuthOptionsFromEnv()
	if err != nil{
		fmt.Errorf("Unable to get the Auth options from environment variables: %s", err)
		return nil, err
	}

	Provider, err := openstack.AuthenticatedClient(AuthOptions)
	if err != nil {
		fmt.Errorf("Unable to get provider: %s", err)
		return nil, err
	}

	p.ZunClient, err = openstack.NewContainerV1(Provider, gophercloud.EndpointOpts{
		Region: os.Getenv("OS_REGION_NAME"),
	})
	if err != nil {
		fmt.Errorf("Unable to get zun client")
		return nil, err
	}

	// Set sane defaults for Capacity in case config is not supplied
	p.cpu = "20"
	p.memory = "100Gi"
	p.pods = "20"

	p.operatingSystem = operatingSystem
	p.nodeName = nodeName
	p.daemonEndpointPort = daemonEndpointPort

	return &p, err
}

// GetPod returns a pod by name that is running inside ACI
// returns nil if a pod by that name is not found.
func (p *ZunProvider) GetPod(namespace, name string) (*v1.Pod, error) {
	capsule, err := capsules.Get(p.ZunClient, fmt.Sprintf("%s-%s", namespace, name)).Extract()
	if err != nil {
		return nil, err
	}

	//if cg.Tags["NodeName"] != p.nodeName {
	//	return nil, nil
	//}

	return capsuleToPod(capsule)
}

// GetPods returns a list of all pods known to be running within ACI.
func (p *ZunProvider) GetPods() ([]*v1.Pod, error) {
        pager := capsules.List(p.ZunClient, nil)

	pages := 0
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		pages++
		return true, nil
	})
        if err != nil {
                return nil, err
        }

        pods := make([]*v1.Pod, 0, pages)
        err = pager.EachPage(func(page pagination.Page) (bool, error) {
                CapsuleList, err := capsules.ExtractCapsules(page)
                if err != nil {
                        return false, err
                }

                for _, m := range CapsuleList {
			c := m
			if m.MetaLabels["NodeName"] != p.nodeName {
				continue
			}
			p, err := capsuleToPod(&c)
			if err != nil {
		                log.Println(err)
				continue
	                }
			pods = append(pods, p)
		}
		return true, nil
        })
        if err != nil {
                return nil, err
        }
        return pods, nil
}

// CreatePod accepts a Pod definition and creates
// an Zun deployment
func (p *ZunProvider) CreatePod(pod *v1.Pod) error {
	//capsuleTemplate := new(capsules.Template)
	var capsule capsules.Capsule
	capsule.RestartPolicy = pod.Spec.RestartPolicy
	capsule.CapsuleVersion = "beta"

	podUID := string(pod.UID)
	podCreationTimestamp := pod.CreationTimestamp.String()
	capsule.MetaLabels = map[string]string{
		"PodName":           pod.Name,
		"ClusterName":       pod.ClusterName,
		"NodeName":          pod.Spec.NodeName,
		"Namespace":         pod.Namespace,
		"UID":               podUID,
		"CreationTimestamp": podCreationTimestamp,
	}
	capsule.MetaName = pod.Namespace + '-' + pod.Name


	// get containers
	containers, err := p.getContainers(pod)
	if err != nil {
		return err
	}

	// assign all the things
	capsules.Capsule.Containers = containers

	// TODO(BJK) containergrouprestartpolicy??
	_, err = p.aciClient.CreateContainerGroup(
		p.resourceGroup,
		fmt.Sprintf("%s-%s", pod.Namespace, pod.Name),
		containerGroup,
	)

	return err
}

func (p *ZunProvider) getContainers(pod *v1.Pod) ([]capsules.Container, error) {
	containers := make([]capsules.Container, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		c := capsules.Container{
			Name: container.Name,
			Image: container.Image,
			Command: append(container.Command, container.Args...),
			WorkDir: container.WorkingDir,
			ImagePullPolicy: container.ImagePullPolicy,
		}

		c.Environment = map[string]string{}
		for _, e := range container.Env {
			c.Environment[e.Name] = e.Value
		}

		if container.Resources.Limits != nil {
		//	cpuLimit := cpuRequest
			if _, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
				cpuLimit = float64(container.Resources.Limits.Cpu().MilliValue()) / 1000.00
			}

		//	memoryLimit := memoryRequest
			if _, ok := container.Resources.Limits[v1.ResourceMemory]; ok {
				memoryLimit = float64(container.Resources.Limits.Memory().Value()) / 1000000000.00
			}

			c.CPU = cpuLimit
			c.Memory = memoryLimit*1024
		}

		// NOTE(kevinz): Zun cpu request not support
		//		cpuRequest := 1.00
		//		if _, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		//			cpuRequest = float64(container.Resources.Requests.Cpu().MilliValue()/10.00) / 100.00
		//			if cpuRequest < 0.01 {
		//				cpuRequest = 0.01
		//			}
		//		}

		// NOTE(kevinz): Zun memory request not support
		//		memoryRequest := 1.50
		//		if _, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
		//			memoryRequest = float64(container.Resources.Requests.Memory().Value()/100000000.00) / 10.00
		//			if memoryRequest < 0.10 {
		//				memoryRequest = 0.10
		//			}
		//		}

		//		c.Resources = aci.ResourceRequirements{
		//			Requests: &aci.ResourceRequests{
		//				CPU:        cpuRequest,
		//				MemoryInGB: memoryRequest,
		//			},
		//		}

		//Sync Port with container
		//		for _, p := range container.Ports {
		//			c.Ports = append(c.Ports, aci.ContainerPort{
		//				Port:     p.ContainerPort,
		//				Protocol: getProtocol(p.Protocol),
		//			})
		//		}

		//Add later for volume
		//		c.VolumeMounts = make([]aci.VolumeMount, 0, len(container.VolumeMounts))
		//		for _, v := range container.VolumeMounts {
		//			c.VolumeMounts = append(c.VolumeMounts, aci.VolumeMount{
		//				Name:      v.Name,
		//				MountPath: v.MountPath,
		//				ReadOnly:  v.ReadOnly,
		//			})
		//		}
		containers = append(containers, c)
	}
	return containers, nil
}

// GetPodStatus returns the status of a pod by name that is running inside ACI
// returns nil if a pod by that name is not found.
func (p *ZunProvider) GetPodStatus(namespace, name string) (*v1.PodStatus, error) {
	pod, err := p.GetPod(namespace, name)
	if err != nil {
		return nil, err
	}

	if pod == nil {
		return nil, nil
	}

	return &pod.Status, nil
}

func (p *ZunProvider) GetContainerLogs(namespace, podName, containerName string, tail int) (string, error) {
	return "not support in Zun Provider", nil
}

// NodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status
// within Kubernetes.
func (p *ZunProvider) NodeConditions() []v1.NodeCondition {
	// TODO: Make these dynamic and augment with custom ACI specific conditions of interest
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

// NodeAddresses returns a list of addresses for the node status
// within Kubernetes.
func (p *ZunProvider) NodeAddresses() []v1.NodeAddress {
	return nil
}

// NodeDaemonEndpoints returns NodeDaemonEndpoints for the node status
// within Kubernetes.
func (p *ZunProvider) NodeDaemonEndpoints() *v1.NodeDaemonEndpoints {
	return &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: p.daemonEndpointPort,
		},
	}
}

// OperatingSystem returns the operating system for this provider.
// This is a noop to default to Linux for now.
func (p *ZunProvider) OperatingSystem() string {
	return providers.OperatingSystemLinux
}

func capsuleToPod(capsule *capsules.Capsule) (*v1.Pod, error) {
	var podCreationTimestamp metav1.Time

	podCreationTimestamp = metav1.NewTime(capsule.CreatedAt)
	//Zun don't record capsule start time, use update time instead
	//containerStartTime := metav1.NewTime(time.Time(cg.Containers[0].ContainerProperties.InstanceView.CurrentState.StartTime))
	containerStartTime := metav1.NewTime(capsule.UpdatedAt)

	// Deal with container inside capsule
	containers := make([]v1.Container, 0, len(capsule.Containers))
	containerStatuses := make([]v1.ContainerStatus, 0, len(capsule.Containers))
	for _, c := range capsule.Containers {
		containerCommand := []string{c.Command}
		//containerMemoryMB, err := strconv.Atoi(c.Memory)
		containerMemoryMB, err := strconv.Atoi("1024")
		if err != nil{
			log.Println(err)
		}
		containerMemoryGB := float64(containerMemoryMB/1024)
		container := v1.Container{
			Name:    c.Name,
			Image:   c.Image,
			Command: containerCommand,
			Resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", int64(c.CPU))),
					v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%gG", containerMemoryGB)),
				},
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", int64(c.CPU*1024/100))),
					v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%gG", containerMemoryGB)),
				},
			},
		}
		containers = append(containers, container)
		containerStatus := v1.ContainerStatus{
			Name:                 c.Name,
			State:                zunContainerStausToContainerStatus(&c),
			//Zun doesn't record termination state.
			LastTerminationState: zunContainerStausToContainerStatus(&c),
			Ready:                zunStatusToPodPhase(c.Status) == v1.PodRunning,
			//Zun doesn't record restartCount.
			RestartCount:         int32(0),
			Image:                c.Image,
			ImageID:              "",
			ContainerID:          c.ContainerID,
		}

		// Add to containerStatuses
		containerStatuses = append(containerStatuses, containerStatus)
	}

	ip := ""
	if capsule.Addresses != nil {
		for _, v := range capsule.Addresses {
			for _, addr := range v {
				if addr.Version == float64(4) {
					ip = addr.Addr
				}
			}
		}
	}

	p := v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              capsule.MetaLabels["PodName"],
			Namespace:         capsule.MetaLabels["Namespace"],
			ClusterName:       capsule.MetaLabels["ClusterName"],
			UID:               types.UID(capsule.UUID),
			CreationTimestamp: podCreationTimestamp,
		},
		Spec: v1.PodSpec{
			NodeName:   capsule.MetaLabels["NodeName"],
			Volumes:    []v1.Volume{},
			Containers: containers,
		},

		Status: v1.PodStatus{
			Phase:             zunCapStatusToPodPhase(capsule.Status),
			Conditions:        []v1.PodCondition{},
			Message:           "",
			Reason:            "",
			HostIP:            "",
			PodIP:             ip,
			StartTime:         &containerStartTime,
			ContainerStatuses: containerStatuses,
		},
	}

	return &p, nil
}

// UpdatePod is a noop, Zun currently does not support live updates of a pod.
func (p *ZunProvider) UpdatePod(pod *v1.Pod) error {
	return nil
}

// DeletePod deletes the specified pod out of Zun.
func (p *ZunProvider) DeletePod(pod *v1.Pod) error {
	return capsules.Delete(p.ZunClient, fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)).ExtractErr()
}

func zunContainerStausToContainerStatus(cs *capsules.Container) v1.ContainerState {
	// Zun already container start time but not add support at gophercloud
	//startTime := metav1.NewTime(time.Time(cs.StartTime))

	// Zun container status:
	//'Error', 'Running', 'Stopped', 'Paused', 'Unknown', 'Creating', 'Created',
	//'Deleted', 'Deleting', 'Rebuilding', 'Dead', 'Restarting'

	// Handle the case where the container is running.
	if cs.Status == "Running" || cs.Status == "Stopped"{
		return v1.ContainerState{
			Running: &v1.ContainerStateRunning{
				StartedAt: metav1.NewTime(time.Time(cs.CreatedAt)),
			},
		}
	}

	// Handle the case where the container failed.
	if cs.Status == "Error" || cs.Status == "Dead" {
		return v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				//ExitCode:   cs.ExitCode,
				ExitCode:   int32(0),
				Reason:     cs.Status,
				Message:    cs.StatusDetail,
				//StartedAt:  startTime,
				StartedAt:  metav1.NewTime(time.Time(cs.CreatedAt)),
				//Zun doesn't have FinishAt
				FinishedAt: metav1.NewTime(time.Time(cs.UpdatedAt)),
			},
		}
	}

	// Handle the case where the container is pending.
	// Which should be all other aci states.
	return v1.ContainerState{
		Waiting: &v1.ContainerStateWaiting{
			Reason:  cs.Status,
			Message: cs.StatusDetail,
		},
	}
}

func zunStatusToPodPhase(status string) v1.PodPhase {
	switch status {
	case "Running":
		return v1.PodRunning
	case "Stopped":
		return v1.PodSucceeded
	case "Error":
		return v1.PodFailed
	case "Dead":
		return v1.PodFailed
	case "Creating":
		return v1.PodPending
	case "Created":
		return v1.PodPending
	case "Restarting":
		return v1.PodPending
	case "Rebuilding":
		return v1.PodPending
	case "Paused":
		return v1.PodPending
	case "Deleting":
		return v1.PodPending
	case "Deleted":
		return v1.PodPending
	}

	return v1.PodUnknown
}

func zunCapStatusToPodPhase(status string) v1.PodPhase {
	switch status {
	case "Running":
		return v1.PodRunning
	case "Succeeded":
		return v1.PodSucceeded
	case "Failed":
		return v1.PodFailed
	case "Pending":
		return v1.PodPending
	}

	return v1.PodUnknown
}

// Capacity returns a resource list containing the capacity limits set for ACI.
func (p *ZunProvider) Capacity() v1.ResourceList {
	return v1.ResourceList{
		"cpu":    resource.MustParse(p.cpu),
		"memory": resource.MustParse(p.memory),
		"pods":   resource.MustParse(p.pods),
	}
}

