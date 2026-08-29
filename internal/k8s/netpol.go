package k8s

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func toNetworkPolicy(p *NetworkPolicy) *networkingv1.NetworkPolicy {
	ns := Namespace
	out := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: ns,
			Labels: map[string]string{
				LabelWorkload:  WorkloadSandbox,
				LabelSandboxID: p.SandboxID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{LabelSandboxID: p.SandboxID},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ignition-system"},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "ignition-gateway"},
					},
				}},
			}},
		},
	}
	if p.AllowDNS {
		dnsPort := intstr.FromInt(53)
		udp := corev1.ProtocolUDP
		tcp := corev1.ProtocolTCP
		out.Spec.Egress = append(out.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &dnsPort},
				{Protocol: &tcp, Port: &dnsPort},
			},
		})
	}
	for _, cidr := range p.EgressCIDRs {
		c := cidr
		out.Spec.Egress = append(out.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: c},
			}},
		})
	}
	return out
}
