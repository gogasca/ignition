package k8s

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func toCorev1(p *Pod) (*corev1.Pod, error) {
	if p == nil {
		return nil, fmt.Errorf("pod is nil")
	}
	for _, v := range p.Spec.Volumes {
		if v.HostPath != "" {
			return nil, fmt.Errorf("hostPath is forbidden")
		}
	}
	ns := p.Namespace
	if ns == "" {
		ns = Namespace
	}
	out := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        p.Name,
			Namespace:   ns,
			Labels:      cloneMap(p.Labels),
			Annotations: cloneMap(p.Annotations),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicy(p.Spec.RestartPolicy),
			PriorityClassName:             p.Spec.PriorityClassName,
			NodeSelector:                  cloneMap(p.Spec.NodeSelector),
			AutomountServiceAccountToken:  p.Spec.AutomountServiceAccountToken,
			EnableServiceLinks:            p.Spec.EnableServiceLinks,
			ActiveDeadlineSeconds:         p.Spec.ActiveDeadlineSeconds,
			TerminationGracePeriodSeconds: p.Spec.TerminationGraceSeconds,
		},
	}
	if p.Spec.RuntimeClassName != "" {
		name := p.Spec.RuntimeClassName
		out.Spec.RuntimeClassName = &name
	}
	if p.Spec.AntiAffinityHostname {
		out.Spec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{LabelWorkload: WorkloadSandbox},
					},
				}},
			},
		}
	}
	for _, t := range p.Spec.Tolerations {
		out.Spec.Tolerations = append(out.Spec.Tolerations, corev1.Toleration{
			Key:      t.Key,
			Operator: corev1.TolerationOperator(t.Operator),
			Value:    t.Value,
			Effect:   corev1.TaintEffect(t.Effect),
		})
	}
	for _, c := range p.Spec.Containers {
		ctr, err := toContainer(c, p.Spec)
		if err != nil {
			return nil, err
		}
		out.Spec.Containers = append(out.Spec.Containers, ctr)
	}
	for _, v := range p.Spec.Volumes {
		vol := corev1.Volume{Name: v.Name}
		if v.EmptyDir {
			ed := &corev1.EmptyDirVolumeSource{}
			if v.SizeLimit != "" {
				q, err := resource.ParseQuantity(v.SizeLimit)
				if err != nil {
					return nil, fmt.Errorf("volume %s sizeLimit: %w", v.Name, err)
				}
				ed.SizeLimit = &q
			}
			vol.EmptyDir = ed
		}
		out.Spec.Volumes = append(out.Spec.Volumes, vol)
	}
	return out, nil
}

func toContainer(c Container, spec PodSpec) (corev1.Container, error) {
	cpu := resource.NewMilliQuantity(int64(c.CPUMilli), resource.DecimalSI)
	mem := resource.NewQuantity(int64(c.MemoryMiB)*1024*1024, resource.BinarySI)
	priv := c.AllowPrivEsc
	ctr := corev1.Container{
		Name:       c.Name,
		Image:      c.Image,
		Command:    append([]string{}, c.Command...),
		WorkingDir: c.WorkingDir,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *cpu,
				corev1.ResourceMemory: *mem,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    cpu.DeepCopy(),
				corev1.ResourceMemory: mem.DeepCopy(),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &priv,
			RunAsNonRoot:             boolPtr(spec.RunAsNonRoot),
		},
	}
	if c.GPU != "" {
		gpu, err := resource.ParseQuantity(c.GPU)
		if err != nil {
			return corev1.Container{}, fmt.Errorf("gpu quantity: %w", err)
		}
		ctr.Resources.Requests[corev1.ResourceName(GPUResource)] = gpu
		ctr.Resources.Limits[corev1.ResourceName(GPUResource)] = gpu.DeepCopy()
	}
	if spec.SeccompRuntimeDefault {
		t := corev1.SeccompProfileTypeRuntimeDefault
		ctr.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: t}
	}
	if c.DropAllCaps {
		ctr.SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	}
	if c.ReadOnlyRootFS {
		ctr.SecurityContext.ReadOnlyRootFilesystem = boolPtr(true)
	}
	if c.Port > 0 {
		ctr.Ports = append(ctr.Ports, corev1.ContainerPort{Name: "supervisor", ContainerPort: int32(c.Port)})
	}
	probe := func(path string) *corev1.Probe {
		if path == "" || c.Port <= 0 {
			return nil
		}
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt(c.Port),
			}},
			PeriodSeconds:    5,
			TimeoutSeconds:   2,
			FailureThreshold: 3,
		}
	}
	ctr.LivenessProbe = probe(c.LivenessPath)
	ctr.ReadinessProbe = probe(c.ReadinessPath)
	for k, v := range c.Env {
		ctr.Env = append(ctr.Env, corev1.EnvVar{Name: k, Value: v})
	}
	if c.VolumeMountPath != "" && len(spec.Volumes) > 0 {
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{
			Name:      spec.Volumes[0].Name,
			MountPath: c.VolumeMountPath,
		})
	}
	return ctr, nil
}

func fromCorev1(p *corev1.Pod) *Pod {
	if p == nil {
		return nil
	}
	out := &Pod{
		Name:        p.Name,
		Namespace:   p.Namespace,
		NodeName:    p.Spec.NodeName,
		Labels:      cloneMap(p.Labels),
		Annotations: cloneMap(p.Annotations),
		Phase:       string(p.Status.Phase),
		Reason:      p.Status.Reason,
		Deleting:    p.DeletionTimestamp != nil,
	}
	if out.NodeName != "" {
		out.Scheduled = true
	}
	for _, c := range p.Status.Conditions {
		switch c.Type {
		case corev1.PodScheduled:
			if c.Status == corev1.ConditionTrue {
				out.Scheduled = true
			}
		case corev1.PodReady:
			if c.Status == corev1.ConditionTrue {
				out.Ready = true
			}
		}
	}
	if p.Status.Phase == corev1.PodRunning {
		out.Running = true
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			out.Reason = cs.State.Waiting.Reason
		}
	}
	return out
}

func ToCorev1(p *Pod) (*corev1.Pod, error) { return toCorev1(p) }

func FromCorev1(p *corev1.Pod) *Pod { return fromCorev1(p) }

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
