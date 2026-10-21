package kube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"kubeflow-admin/internal/model"
)

func display(v any) string {
	if v == nil {
		return "(absent)"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func (w *work) change(field string, before, after any) {
	b, a := display(before), display(after)
	if b != a {
		w.prepared.Changes = append(w.prepared.Changes, model.Change{Field: field, Before: b, After: a})
	}
}
func (s *Service) previews(w *work) {
	a := w.prepared.Action
	switch a.Kind {
	case "profile.create":
		w.change("profile", nil, a.Name)
		w.change("owner", nil, a.Owner)
		for k, v := range a.Quota {
			w.change("quota."+k, nil, v)
		}
	case "profile.quota.change":
		old, _, _ := unstructured.NestedMap(w.profile.Object, "spec", "resourceQuotaSpec", "hard")
		for k, v := range a.Quota {
			w.change("quota."+k, old[k], v)
		}
	case "pvc.expand":
		w.change("requested storage", w.pvc.Spec.Resources.Requests.Storage().String(), a.Size)
	case "pvc.create.dynamic", "storage.create.static", "pv.reserve-and-claim":
		w.change("claim", nil, a.Namespace+"/"+a.Name)
		w.change("requested storage", nil, a.Size)
		w.change("storage class", nil, a.StorageClass)
		w.change("access mode", nil, a.AccessMode)
		if a.PVName != "" {
			w.change("reserved PV", nil, a.PVName)
		}
		if a.Template != "" {
			w.change("approved storage template", nil, a.Template)
		}
	case "notebook.start", "notebook.stop":
		_, stop := w.notebook.GetAnnotations()[stoppedKey]
		before := "Running"
		if stop {
			before = "Stopped"
		}
		after := "Running"
		if a.Kind == "notebook.stop" {
			after = "Stopped"
		}
		w.change("desired lifecycle", before, after)
	case "notebook.edit":
		keys := []string{"cpu", "memory"}
		for _, p := range s.Config.Notebooks.GPUPresets {
			keys = append(keys, p.ResourceKey)
		}
		for _, op := range w.ops {
			parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
			old, _ := valueAt(w.notebook.Object, parts)
			after := op.Value
			if op.Op == "remove" {
				after = nil
			}
			if strings.HasSuffix(op.Path, "/resources") {
				for _, group := range []string{"requests", "limits"} {
					for _, key := range keys {
						var before, v any
						if m, ok := old.(map[string]any); ok {
							before, _, _ = unstructured.NestedFieldNoCopy(m, group, key)
						}
						if m, ok := after.(map[string]any); ok {
							v, _, _ = unstructured.NestedFieldNoCopy(m, group, key)
						}
						w.change(fmt.Sprintf("container %s %s.%s", a.Container, group, key), before, v)
					}
				}
			} else if strings.HasSuffix(op.Path, "/image") {
				w.change("container "+a.Container+" image", old, after)
			} else if strings.HasSuffix(op.Path, "/volumeMounts") {
				w.change("container "+a.Container+" PVC mounts", safeMountPreview(w.notebook, old), a.Mounts)
			} else if op.Path == "/spec/template/spec/nodeSelector" {
				allowed := map[string]bool{}
				for _, p := range s.Config.Notebooks.GPUPresets {
					for k := range p.NodeSelector {
						allowed[k] = true
					}
				}
				for k := range allowed {
					var before, v any
					if m, ok := old.(map[string]any); ok {
						before = m[k]
					}
					if m, ok := after.(map[string]any); ok {
						v = m[k]
					}
					w.change("GPU node selector "+k, before, v)
				}
			} else if op.Path == "/spec/template/spec/runtimeClassName" {
				w.change("RuntimeClass", old, after)
			}
		}
		_, stopped := w.notebook.GetAnnotations()[stoppedKey]
		before, after := "Running", "Stopped"
		if stopped {
			before = "Stopped"
		}
		if a.Restart && !stopped {
			after = "Running"
		}
		w.change("desired lifecycle", before, after)
	case "pod.restart":
		w.change("target Pod", nil, w.pod.Namespace+"/"+w.pod.Name)
		w.change("controller", nil, "StatefulSet/"+w.podSet.Name)
		w.change("parent Notebook", nil, w.podNotebook.GetNamespace()+"/"+w.podNotebook.GetName())
		w.change("current revision", nil, w.pod.Labels[appsv1.StatefulSetRevisionLabel])
		w.change("desired revision", nil, w.podSet.Status.UpdateRevision)
		w.change("assigned Node", nil, w.podNode.Name)
		w.change("disruption handling", nil, "policy/v1 eviction with UID/resourceVersion preconditions; PodDisruptionBudget enforced by the API")
	case "node.cordon", "node.uncordon":
		want := a.Kind == "node.cordon"
		w.change("spec.unschedulable", strconv.FormatBool(w.node.Spec.Unschedulable), strconv.FormatBool(want))
		w.change("status", nodeState(w.node), nodeDesiredState(w.node, want))
		w.change("force", "false", strconv.FormatBool(a.Force))
	}
	sort.SliceStable(w.prepared.Changes, func(i, j int) bool { return w.prepared.Changes[i].Field < w.prepared.Changes[j].Field })
}
func safeMountPreview(n *unstructured.Unstructured, raw any) []model.Mount {
	out := []model.Mount{}
	vs, _, _ := unstructured.NestedSlice(n.Object, "spec", "template", "spec", "volumes")
	claims := map[string]string{}
	for _, v := range vs {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		claim, found, _ := unstructured.NestedString(m, "persistentVolumeClaim", "claimName")
		if found {
			claims[fmt.Sprint(m["name"])] = claim
		}
	}
	mounts, _ := raw.([]any)
	for _, v := range mounts {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if claim, ok := claims[name]; ok {
			p, _ := m["mountPath"].(string)
			ro, _ := m["readOnly"].(bool)
			out = append(out, model.Mount{Name: name, Claim: claim, Path: p, ReadOnly: ro})
		}
	}
	return out
}
