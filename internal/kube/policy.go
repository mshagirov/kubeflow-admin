package kube

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// effectivePodBudget implements ordinary init-container maxima and Pod
// overhead. Restartable init sidecars are deliberately unsupported rather
// than underestimated. Admission-injected sidecars remain a live limitation.
func effectivePodBudget(p corev1.PodSpec) (corev1.ResourceList, error) {
	result := corev1.ResourceList{}
	add := func(group string, rs corev1.ResourceList) {
		for k, v := range rs {
			key := corev1.ResourceName(group + string(k))
			q := result[key]
			q.Add(v)
			result[key] = q
		}
	}
	for _, c := range p.Containers {
		add("requests.", c.Resources.Requests)
		add("limits.", c.Resources.Limits)
	}
	for _, c := range p.InitContainers {
		if c.RestartPolicy != nil {
			return nil, errors.New("restartable init sidecar resource accounting requires a tested admission adapter")
		}
		for _, group := range []struct {
			prefix string
			rs     corev1.ResourceList
		}{{"requests.", c.Resources.Requests}, {"limits.", c.Resources.Limits}} {
			for k, v := range group.rs {
				key := corev1.ResourceName(group.prefix + string(k))
				q := result[key]
				if v.Cmp(q) > 0 {
					result[key] = v.DeepCopy()
				}
			}
		}
	}
	for k, v := range p.Overhead {
		for _, prefix := range []string{"requests.", "limits."} {
			key := corev1.ResourceName(prefix + string(k))
			q := result[key]
			if prefix == "requests." || q.Sign() > 0 {
				q.Add(v)
				result[key] = q
			}
		}
	}
	result[corev1.ResourcePods] = resource.MustParse("1")
	return result, nil
}
func applyResourceDefaults(p *corev1.PodSpec, ls []corev1.LimitRange) error {
	req, lim := corev1.ResourceList{}, corev1.ResourceList{}
	for _, l := range ls {
		for _, v := range l.Spec.Limits {
			if v.Type != corev1.LimitTypeContainer {
				continue
			}
			for k, q := range v.DefaultRequest {
				if old, ok := req[k]; ok && old.Cmp(q) != 0 {
					return errors.New("ambiguous LimitRange request defaults")
				}
				req[k] = q.DeepCopy()
			}
			for k, q := range v.Default {
				if old, ok := lim[k]; ok && old.Cmp(q) != 0 {
					return errors.New("ambiguous LimitRange limit defaults")
				}
				lim[k] = q.DeepCopy()
			}
		}
	}
	fill := func(c *corev1.Container) {
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		for k, q := range lim {
			if _, exists := c.Resources.Limits[k]; !exists {
				c.Resources.Limits[k] = q.DeepCopy()
			}
		}
		for k, q := range req {
			if _, exists := c.Resources.Requests[k]; !exists {
				c.Resources.Requests[k] = q.DeepCopy()
			}
		}
		for k, q := range c.Resources.Limits {
			if _, exists := c.Resources.Requests[k]; !exists {
				c.Resources.Requests[k] = q.DeepCopy()
			}
		}
	}
	for i := range p.Containers {
		fill(&p.Containers[i])
	}
	for i := range p.InitContainers {
		fill(&p.InitContainers[i])
	}
	return nil
}
