package k8s

import "errors"

const (
	Namespace = "ignition-sandboxes"

	LabelWorkload  = "ignition.io/workload"
	LabelSandboxID = "ignition.io/sandbox-id"
	LabelProjectID = "ignition.io/project-id"

	WorkloadSandbox = "gpu-sandbox"
	WorkloadBalloon = "gpu-balloon"

	AnnotInitHealthy  = "ignition.io/init-healthy"
	AnnotGPUUUID      = "ignition.io/gpu-uuid"
	AnnotGPUCleanup   = "ignition.io/gpu-cleanup"
	AnnotImageID      = "ignition.io/image-id"
	AnnotCommand      = "ignition.io/tenant-command"
	AnnotProcDesired  = "ignition.io/process-desired"
	AnnotProcObserved = "ignition.io/process-observed"

	AnnotScaleDownDisabled = "cluster-autoscaler.kubernetes.io/scale-down-disabled"

	PrioritySandbox = "ignition-sandbox"
	PriorityBalloon = "ignition-balloon"
	RuntimeClass    = "gvisor"
	GPUResource     = "nvidia.com/gpu"

	NodePoolLabel    = "ignition.io/node-pool"
	GPUNodePoolLabel = NodePoolLabel // deprecated alias
	GPUNodePoolValue = "gpu-sandbox-l4"
	CPUNodePoolValue = "cpu-sandbox"
	AnnotGPUType     = "ignition.io/gpu-type"

	// EnvAccelerator tells sandbox-init which accelerator class to verify at
	// readiness. "NONE" means CPU-only: readiness is just the supervisor.
	EnvAccelerator = "IGNITION_ACCELERATOR"
)

var (
	ErrNotFound      = errors.New("pod not found")
	ErrAlreadyExists = errors.New("pod already exists")
	ErrCordonRefused = errors.New("node is not in gpu sandbox pool")
)

// Pod is the controller's view of a cluster Pod. It is not client-go.
type Pod struct {
	Name        string
	Namespace   string
	NodeName    string
	Labels      map[string]string
	Annotations map[string]string
	Spec        PodSpec
	Phase       string
	Reason      string
	Scheduled   bool
	Running     bool
	Ready       bool // kubelet PodReady; Fake SetReady sets this
	Deleting    bool
}

type PodSpec struct {
	RuntimeClassName             string
	PriorityClassName            string
	AutomountServiceAccountToken *bool
	EnableServiceLinks           *bool
	RestartPolicy                string
	ActiveDeadlineSeconds        *int64
	TerminationGraceSeconds      *int64
	NodeSelector                 map[string]string
	Tolerations                  []Toleration
	AntiAffinityHostname         bool
	RunAsNonRoot                 bool
	SeccompRuntimeDefault        bool
	Containers                   []Container
	Volumes                      []Volume
}

type Toleration struct {
	Key, Operator, Value, Effect string
}

type Container struct {
	Name            string
	Image           string
	Command         []string
	Env             map[string]string
	WorkingDir      string
	CPUMilli        int
	MemoryMiB       int
	GPU             string
	AllowPrivEsc    bool
	DropAllCaps     bool
	ReadOnlyRootFS  bool
	VolumeMountPath string
	Port            int
	LivenessPath    string
	ReadinessPath   string
}

type Volume struct {
	Name      string
	EmptyDir  bool
	SizeLimit string
	HostPath  string
}

// Pods is the controller-only Kubernetes surface. Tests use Fake.
type Pods interface {
	Get(name string) (*Pod, error)
	List() ([]Pod, error)
	Create(p *Pod) error
	Delete(name string) error
	PatchAnnotations(name string, annotations map[string]string) error
}

// Nodes is used when GPU cleanup is ambiguous and to pin warm nodes.
type Nodes interface {
	CordonAndDelete(nodeName string) error
	SetScaleDownDisabled(nodeName string, disabled bool) error
	ListGPUPool() ([]string, error)
}
