package kube

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

const activityCheckKey = "notebooks.kubeflow.org/last_activity_check_timestamp"

func notebookFingerprint(n *unstructured.Unstructured) string {
	a := n.GetAnnotations()
	delete(a, stoppedKey)
	delete(a, activityKey)
	delete(a, activityCheckKey)
	return digest(struct {
		UID                 types.UID
		Spec                any
		Labels, Annotations map[string]string
	}{n.GetUID(), n.Object["spec"], n.GetLabels(), a})
}
func stopOps(n *unstructured.Unstructured, stop bool) []patchOp {
	a := n.GetAnnotations()
	_, present := a[stoppedKey]
	if stop {
		if present {
			return nil
		}
		if a == nil {
			return []patchOp{{"add", "/metadata/annotations", map[string]any{stoppedKey: time.Now().UTC().Format(time.RFC3339)}}}
		}
		return []patchOp{{"add", "/metadata/annotations/" + pointer(stoppedKey), time.Now().UTC().Format(time.RFC3339)}}
	}
	ops := []patchOp{}
	if present {
		ops = append(ops, patchOp{"remove", "/metadata/annotations/" + pointer(stoppedKey), nil})
	}
	// v1.10's culler initializes BOTH absent keys to now. Remove stale keys
	// once on explicit start; never continuously refresh or disable idle policy.
	if present {
		for _, key := range []string{activityKey, activityCheckKey} {
			if _, ok := a[key]; ok {
				ops = append(ops, patchOp{"remove", "/metadata/annotations/" + pointer(key), nil})
			}
		}
	}
	return ops
}
func containerIndex(cs []any, n string) (int, error) {
	if n == "" && len(cs) == 1 {
		if _, ok := cs[0].(map[string]any); !ok {
			return 0, errors.New("unsupported container shape")
		}
		return 0, nil
	}
	for i, c := range cs {
		m, ok := c.(map[string]any)
		if !ok {
			return 0, errors.New("unsupported container shape")
		}
		if m["name"] == n {
			return i, nil
		}
	}
	return 0, errors.New("select an existing container by name")
}
func (s *Service) notebookChanges(ctx context.Context, a model.Action, n *unstructured.Unstructured, e *evidence) ([]patchOp, error) {
	spec, found, err := unstructured.NestedMap(n.Object, "spec", "template", "spec")
	if err != nil || !found {
		return nil, errors.New("unsupported Notebook Pod template")
	}
	if a.Kind == "notebook.stop" {
		return stopOps(n, true), nil
	}
	if a.Kind == "notebook.start" {
		_, headroom := n.GetAnnotations()[stoppedKey]
		if err = s.validatePod(ctx, a.Namespace, spec, e, headroom); err != nil {
			return nil, err
		}
		return stopOps(n, false), nil
	}
	cs, ok := spec["containers"].([]any)
	if !ok || len(cs) == 0 {
		return nil, errors.New("unsupported container list")
	}
	idx, err := containerIndex(cs, a.Container)
	if err != nil {
		return nil, err
	}
	c, ok := cs[idx].(map[string]any)
	if !ok {
		return nil, errors.New("unsupported container shape")
	}
	cn, ok := c["name"].(string)
	if !ok || cn == "" {
		return nil, errors.New("unsupported container identity")
	}
	prefix := fmt.Sprintf("/spec/template/spec/containers/%d", idx)
	ops := []patchOp{}
	if a.Image != "" {
		if !has(s.Config.Notebooks.AllowedImages, a.Image) {
			return nil, errors.New("image must exactly match an approved image")
		}
		c["image"] = a.Image
		ops = append(ops, patchOp{"add", prefix + "/image", a.Image})
	}
	resources, _, err := unstructured.NestedMap(c, "resources")
	if err != nil {
		return nil, errors.New("unsupported resources shape")
	}
	if resources == nil {
		resources = map[string]any{}
	}
	original := digest(resources)
	for _, item := range []struct{ group, key, value string }{{"requests", "cpu", a.CPURequest}, {"limits", "cpu", a.CPULimit}, {"requests", "memory", a.MemoryRequest}, {"limits", "memory", a.MemoryLimit}} {
		if item.value == "" {
			continue
		}
		q, err := quantity(item.value)
		if err != nil {
			return nil, err
		}
		m, _, err := unstructured.NestedMap(resources, item.group)
		if err != nil {
			return nil, errors.New("unsupported resource map")
		}
		if m == nil {
			m = map[string]any{}
		}
		m[item.key] = q.String()
		resources[item.group] = m
	}
	if a.GPUPreset != "" || a.GPUCount != nil {
		gpuOps, err := s.gpuChanges(ctx, a, n, spec, c, resources, e)
		if err != nil {
			return nil, err
		}
		ops = append(ops, gpuOps...)
	}
	if digest(resources) != original {
		c["resources"] = resources
		ops = append(ops, patchOp{"add", prefix + "/resources", resources})
	}
	if a.Mounts != nil {
		mounts, volumes, err := s.mountChanges(a, n, spec, idx)
		if err != nil {
			return nil, err
		}
		c["volumeMounts"] = mounts
		spec["volumes"] = volumes
		ops = append(ops, patchOp{"add", prefix + "/volumeMounts", mounts}, patchOp{"add", "/spec/template/spec/volumes", volumes})
	}
	if len(ops) == 0 {
		return nil, errors.New("no supported Notebook edit was requested")
	}
	cs[idx] = c
	spec["containers"] = cs
	_, headroom := n.GetAnnotations()[stoppedKey]
	if err = s.validatePod(ctx, a.Namespace, spec, e, headroom); err != nil {
		return nil, err
	}
	return ops, nil
}

func (s *Service) mountChanges(a model.Action, n *unstructured.Unstructured, spec map[string]any, idx int) ([]any, []any, error) {
	cs, ok := spec["containers"].([]any)
	if !ok || idx < 0 || idx >= len(cs) {
		return nil, nil, errors.New("unsupported container list")
	}
	c, ok := cs[idx].(map[string]any)
	if !ok {
		return nil, nil, errors.New("unsupported container shape")
	}
	vols, ok := spec["volumes"].([]any)
	if !ok && spec["volumes"] != nil {
		return nil, nil, errors.New("unsupported volumes shape")
	}
	vm, ok := c["volumeMounts"].([]any)
	if !ok && c["volumeMounts"] != nil {
		return nil, nil, errors.New("unsupported mounts shape")
	}
	byName := map[string]map[string]any{}
	for _, v := range vols {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.New("unsupported volume shape")
		}
		name, ok := m["name"].(string)
		if !ok {
			return nil, nil, errors.New("unsupported volume identity")
		}
		if byName[name] != nil {
			return nil, nil, errors.New("duplicate existing volume names")
		}
		byName[name] = m
	}
	old := map[string]map[string]any{}
	out := []any{}
	paths := map[string]bool{}
	managed := map[string]bool{}
	for _, v := range vm {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.New("unsupported mount shape")
		}
		vn, _ := m["name"].(string)
		mp, _ := m["mountPath"].(string)
		volume := byName[vn]
		if volume == nil {
			return nil, nil, errors.New("mount references a missing volume")
		}
		if _, pvc := volume["persistentVolumeClaim"]; pvc {
			if m["subPath"] != nil || m["subPathExpr"] != nil {
				return nil, nil, errors.New("advanced PVC subPath edits are unsupported")
			}
			if old[vn] != nil {
				return nil, nil, errors.New("editing duplicate mounts of one PVC volume is unsupported")
			}
			old[vn] = m
			managed[vn] = true
		} else {
			out = append(out, m)
			paths[mp] = true
		}
	}
	names := map[string]bool{}
	for _, m := range *a.Mounts {
		if len(validation.IsDNS1123Label(m.Name)) > 0 || name(m.Claim) != nil {
			return nil, nil, errors.New("invalid mount/claim name")
		}
		if names[m.Name] || paths[m.Path] {
			return nil, nil, errors.New("duplicate mount name or path")
		}
		if !safeMountPath(m.Path) {
			return nil, nil, errors.New("unsafe mount path")
		}
		names[m.Name] = true
		paths[m.Path] = true
		if existing := byName[m.Name]; existing != nil {
			if !managed[m.Name] {
				return nil, nil, errors.New("cannot replace an unrelated volume")
			}
			pvc, ok := existing["persistentVolumeClaim"].(map[string]any)
			if !ok {
				return nil, nil, errors.New("unsupported PVC volume")
			}
			if pvc["claimName"] != m.Claim {
				return nil, nil, errors.New("swapping a claim under an existing volume name is unsupported; use a new volume name")
			}
		}
		for vn, prev := range old {
			mp, _ := prev["mountPath"].(string)
			if s.protectedPath(mp) {
				pvc, ok := byName[vn]["persistentVolumeClaim"].(map[string]any)
				if !ok {
					return nil, nil, errors.New("unsupported PVC volume")
				}
				if mp == m.Path && (pvc["claimName"] != m.Claim || vn != m.Name) {
					return nil, nil, errors.New("protected workspace mount cannot be replaced")
				}
			}
		}
		mount := map[string]any{"name": m.Name, "mountPath": m.Path, "readOnly": m.ReadOnly}
		if old[m.Name] != nil {
			copied, ok := runtime.DeepCopyJSONValue(old[m.Name]).(map[string]any)
			if !ok {
				return nil, nil, errors.New("unsupported mount shape")
			}
			mount = copied
			mount["mountPath"] = m.Path
			mount["readOnly"] = m.ReadOnly
		}
		out = append(out, mount)
		if byName[m.Name] == nil {
			v := map[string]any{"name": m.Name, "persistentVolumeClaim": map[string]any{"claimName": m.Claim}}
			vols = append(vols, v)
			byName[m.Name] = v
		}
	}
	for vn, m := range old {
		mp, _ := m["mountPath"].(string)
		if s.protectedPath(mp) {
			preserved := false
			for _, want := range *a.Mounts {
				if want.Name == vn && want.Path == mp {
					preserved = true
					oldRO, _ := m["readOnly"].(bool)
					if oldRO != want.ReadOnly {
						return nil, nil, errors.New("protected workspace read-only mode cannot change")
					}
				}
			}
			if !preserved {
				return nil, nil, errors.New("protected workspace mount cannot be removed")
			}
		}
	}
	// Remove only formerly managed PVC volumes no longer referenced by ANY
	// container/init container. Independent/unknown volumes are retained.
	used := map[string]bool{}
	for _, v := range out {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.New("unsupported mount shape")
		}
		vn, ok := m["name"].(string)
		if !ok {
			return nil, nil, errors.New("unsupported mount identity")
		}
		used[vn] = true
	}
	for i, raw := range cs {
		if i == idx {
			continue
		}
		if err := mountRefs(raw, used); err != nil {
			return nil, nil, err
		}
	}
	if init, ok := spec["initContainers"].([]any); ok {
		for _, raw := range init {
			if err := mountRefs(raw, used); err != nil {
				return nil, nil, err
			}
		}
	}
	if spec["ephemeralContainers"] != nil {
		return nil, nil, errors.New("mount editing ephemeral-container templates is unsupported")
	}
	final := []any{}
	for _, raw := range vols {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, errors.New("unsupported volume shape")
		}
		vn, ok := m["name"].(string)
		if !ok {
			return nil, nil, errors.New("unsupported volume identity")
		}
		if !managed[vn] || used[vn] {
			final = append(final, m)
		}
	}
	return out, final, nil
}
func mountRefs(raw any, used map[string]bool) error {
	c, ok := raw.(map[string]any)
	if !ok {
		return errors.New("unsupported container shape")
	}
	ms, ok := c["volumeMounts"].([]any)
	if !ok && c["volumeMounts"] != nil {
		return errors.New("unsupported mount shape")
	}
	for _, raw := range ms {
		m, ok := raw.(map[string]any)
		if !ok {
			return errors.New("unsupported mount shape")
		}
		n, ok := m["name"].(string)
		if !ok {
			return errors.New("unsupported mount identity")
		}
		used[n] = true
	}
	return nil
}
func safeMountPath(p string) bool {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" || strings.Contains(p, "\\") {
		return false
	}
	for _, v := range []string{"/proc", "/sys", "/dev", "/etc", "/var/run/secrets"} {
		if p == v || strings.HasPrefix(p, v+"/") {
			return false
		}
	}
	return true
}
func (s *Service) protectedPath(p string) bool {
	xs := append([]string{"/home/jovyan"}, s.Config.Notebooks.ProtectedMountPaths...)
	for _, v := range xs {
		if p == v || strings.HasPrefix(p, v+"/") || strings.HasPrefix(v, p+"/") {
			return true
		}
	}
	return false
}

func (s *Service) gpuChanges(ctx context.Context, a model.Action, n *unstructured.Unstructured, spec, c, res map[string]any, e *evidence) ([]patchOp, error) {
	if a.GPUCount == nil || *a.GPUCount < 0 || *a.GPUCount > 1024 {
		return nil, errors.New("GPU edit requires a nonnegative bounded integer count")
	}
	var next, old *config.GPUPreset
	cn, ok := c["name"].(string)
	if !ok {
		return nil, errors.New("unsupported container identity")
	}
	ownerKey := "admin.kubeflow.org/gpu-preset-" + cn
	prev := n.GetAnnotations()[ownerKey]
	for i := range s.Config.Notebooks.GPUPresets {
		p := &s.Config.Notebooks.GPUPresets[i]
		if p.Name == a.GPUPreset {
			if next != nil {
				return nil, errors.New("ambiguous GPU preset")
			}
			next = p
		}
		if p.Name == prev {
			old = p
		}
	}
	if prev != "" && old == nil {
		return nil, errors.New("previous owned GPU preset is no longer configured")
	}
	if *a.GPUCount > 0 && next == nil {
		return nil, errors.New("approved GPU preset required")
	}
	if *a.GPUCount == 0 {
		next = nil
	}
	containers, ok := spec["containers"].([]any)
	if !ok {
		return nil, errors.New("unsupported container list")
	}
	for _, raw := range containers {
		cc, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("unsupported container shape")
		}
		if cc["name"] != cn {
			for _, p := range s.Config.Notebooks.GPUPresets {
				for _, group := range []string{"requests", "limits"} {
					v, found, _ := unstructured.NestedString(cc, "resources", group, p.ResourceKey)
					if found && v != "0" {
						return nil, errors.New("GPU edits with GPU-allocated sidecars are unsupported")
					}
				}
			}
		}
	}
	selectors, _, err := unstructured.NestedStringMap(spec, "nodeSelector")
	if err != nil {
		return nil, errors.New("unsupported nodeSelector shape")
	}
	if selectors == nil {
		selectors = map[string]string{}
	}
	runtimeName, _ := spec["runtimeClassName"].(string)
	if old != nil {
		for k, v := range old.NodeSelector {
			if selectors[k] != v {
				return nil, errors.New("owned GPU selector was independently changed")
			}
			delete(selectors, k)
		}
		if old.RuntimeClassName != "" {
			if runtimeName != old.RuntimeClassName {
				return nil, errors.New("owned GPU RuntimeClass was independently changed")
			}
			runtimeName = ""
		}
	}
	for _, p := range s.Config.Notebooks.GPUPresets {
		for _, group := range []string{"requests", "limits"} {
			m, _, err := unstructured.NestedMap(res, group)
			if err != nil {
				return nil, errors.New("unsupported GPU resource shape")
			}
			if m == nil {
				m = map[string]any{}
			}
			if value, exists := m[p.ResourceKey]; exists {
				if old == nil || old.ResourceKey != p.ResourceKey {
					q, err := resource.ParseQuantity(fmt.Sprint(value))
					if err != nil || q.Sign() != 0 {
						return nil, errors.New("existing GPU allocation has no verified preset ownership")
					}
				}
				delete(m, p.ResourceKey)
			}
			res[group] = m
		}
	}
	if next != nil {
		if len(validation.IsQualifiedName(next.ResourceKey)) > 0 || !strings.Contains(next.ResourceKey, "/") {
			return nil, errors.New("GPU preset requires a valid extended-resource key")
		}
		for k, v := range next.NodeSelector {
			if _, exists := selectors[k]; exists {
				return nil, errors.New("GPU preset would overwrite an independent node selector")
			}
			selectors[k] = v
		}
		if next.RuntimeClassName != "" {
			if runtimeName != "" {
				return nil, errors.New("GPU preset would overwrite an independent RuntimeClass")
			}
			runtimeName = next.RuntimeClassName
		}
		for _, group := range []string{"requests", "limits"} {
			m := res[group].(map[string]any)
			m[next.ResourceKey] = strconv.FormatInt(*a.GPUCount, 10)
		}
	}
	ops := []patchOp{}
	if len(selectors) > 0 {
		spec["nodeSelector"] = stringMap(selectors)
		ops = append(ops, patchOp{"add", "/spec/template/spec/nodeSelector", stringMap(selectors)})
	} else if _, exists := spec["nodeSelector"]; exists {
		delete(spec, "nodeSelector")
		ops = append(ops, patchOp{"remove", "/spec/template/spec/nodeSelector", nil})
	}
	if runtimeName != "" {
		spec["runtimeClassName"] = runtimeName
		ops = append(ops, patchOp{"add", "/spec/template/spec/runtimeClassName", runtimeName})
	} else if _, exists := spec["runtimeClassName"]; exists {
		delete(spec, "runtimeClassName")
		ops = append(ops, patchOp{"remove", "/spec/template/spec/runtimeClassName", nil})
	}
	if next != nil {
		if n.GetAnnotations() == nil {
			ops = append(ops, patchOp{"add", "/metadata/annotations", map[string]any{ownerKey: next.Name}})
		} else {
			ops = append(ops, patchOp{"add", "/metadata/annotations/" + pointer(ownerKey), next.Name})
		}
	} else if prev != "" {
		ops = append(ops, patchOp{"remove", "/metadata/annotations/" + pointer(ownerKey), nil})
	}
	c["resources"] = res
	// A configured preset covers only selectors and RuntimeClass; never guess
	// affinity/toleration ownership or erase independent constraints.
	if _, ok := spec["affinity"]; ok {
		return nil, errors.New("GPU preset edits with affinity require a separately tested scheduling adapter")
	}
	if err = s.gpuNodes(ctx, spec, res, e); err != nil {
		return nil, err
	}
	return ops, nil
}
func stringMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func (s *Service) gpuNodes(ctx context.Context, spec, res map[string]any, e *evidence) error {
	limits, _, _ := unstructured.NestedMap(res, "limits")
	needed := map[corev1.ResourceName]resource.Quantity{}
	for _, p := range s.Config.Notebooks.GPUPresets {
		if v, ok := limits[p.ResourceKey]; ok {
			q, err := resource.ParseQuantity(fmt.Sprint(v))
			if err != nil {
				return errors.New("invalid GPU quantity")
			}
			if q.Sign() > 0 {
				needed[corev1.ResourceName(p.ResourceKey)] = q
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}
	if spec["nodeName"] != nil || spec["affinity"] != nil || spec["schedulerName"] != nil && spec["schedulerName"] != "default-scheduler" {
		return errors.New("GPU scheduling shape is unsupported")
	}
	var pod corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod); err != nil {
		return errors.New("unsupported Pod scheduling shape")
	}
	if pod.RuntimeClassName != nil {
		rc, err := s.Typed.NodeV1().RuntimeClasses().Get(ctx, *pod.RuntimeClassName, metav1.GetOptions{})
		if err != nil {
			return safe(err)
		}
		e.add("RuntimeClass", "", rc.Name, rc)
		if rc.Scheduling != nil {
			return errors.New("RuntimeClass-added scheduling requires a separately tested adapter")
		}
	}
	nodes, err := s.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	type nodePolicy struct {
		Name                              string
		UID                               types.UID
		Labels                            map[string]string
		Taints                            []corev1.Taint
		Allocatable                       corev1.ResourceList
		Unschedulable, Terminating, Ready bool
	}
	stable := []nodePolicy{}
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		stable = append(stable, nodePolicy{n.Name, n.UID, n.Labels, n.Spec.Taints, n.Status.Allocatable, n.Spec.Unschedulable, n.DeletionTimestamp != nil, ready})
	}
	sort.Slice(stable, func(i, j int) bool { return stable[i].Name < stable[j].Name })
	e.add("NodeList", "", "", stable)
	eligible := false
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable || n.DeletionTimestamp != nil {
			continue
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			continue
		}
		ok := true
		for k, v := range pod.NodeSelector {
			if n.Labels[k] != v {
				ok = false
			}
		}
		for k, q := range needed {
			got := n.Status.Allocatable[k]
			if got.Cmp(q) < 0 {
				ok = false
			}
		}
		for _, t := range n.Spec.Taints {
			if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
				continue
			}
			tolerates := false
			for _, tol := range pod.Tolerations {
				if tol.ToleratesTaint(&t) {
					tolerates = true
				}
			}
			if !tolerates {
				ok = false
			}
		}
		if ok {
			eligible = true
		}
	}
	if !eligible {
		return errors.New("no Ready node satisfies GPU preset, independent selectors, taints and advertised allocation")
	}
	return nil
}

func (s *Service) validatePod(ctx context.Context, ns string, spec map[string]any, e *evidence, enforceHeadroom bool) error {
	var pod corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod); err != nil {
		return errors.New("unsupported Pod template shape")
	}
	if len(pod.Containers) == 0 {
		return errors.New("Notebook requires containers")
	}
	for _, c := range append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...) {
		for key, q := range c.Resources.Requests {
			if q.Sign() < 0 {
				return errors.New("negative resource request")
			}
			if l, ok := c.Resources.Limits[key]; ok && q.Cmp(l) > 0 {
				return errors.New("resource request exceeds limit")
			}
		}
		for key, q := range c.Resources.Limits {
			if q.Sign() < 0 {
				return errors.New("negative resource limit")
			}
			if strings.Contains(string(key), "/") {
				if q.MilliValue()%1000 != 0 {
					return errors.New("extended resources must use integer quantities")
				}
				if rq, ok := c.Resources.Requests[key]; ok && rq.Cmp(q) != 0 {
					return errors.New("extended resource request must equal limit")
				}
			}
		}
	}
	// Explicit LimitRange checks complement (not replace) actual Pod admission.
	limits, err := s.Typed.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	if err = applyResourceDefaults(&pod, limits.Items); err != nil {
		return err
	}
	for _, c := range append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...) {
		for k, q := range c.Resources.Requests {
			lim, exists := c.Resources.Limits[k]
			if exists && q.Cmp(lim) > 0 {
				return errors.New("defaulted resource request exceeds limit")
			}
			if strings.Contains(string(k), "/") && (!exists || q.Cmp(lim) != 0) {
				return errors.New("extended resource requests require an equal limit")
			}
		}
	}
	for _, l := range limits.Items {
		for _, item := range l.Spec.Limits {
			if item.Type == corev1.LimitTypePersistentVolumeClaim {
				continue
			}
			if item.Type != corev1.LimitTypeContainer {
				return errors.New("Pod-wide LimitRange checks require a tested sidecar-aware adapter")
			}
			for _, c := range append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...) {
				for k, min := range item.Min {
					q, ok := c.Resources.Requests[k]
					if !ok {
						q = item.DefaultRequest[k]
					}
					if q.Cmp(min) < 0 {
						return errors.New("resource request below LimitRange minimum")
					}
				}
				for k, max := range item.Max {
					q, ok := c.Resources.Limits[k]
					if !ok {
						q = item.Default[k]
					}
					if q.Sign() > 0 && q.Cmp(max) > 0 {
						return errors.New("resource limit exceeds LimitRange maximum")
					}
				}
				for k, ratio := range item.MaxLimitRequestRatio {
					req := c.Resources.Requests[k]
					lim := c.Resources.Limits[k]
					if req.Sign() == 0 || float64(lim.MilliValue())/float64(req.MilliValue()) > ratio.AsApproximateFloat64() {
						return errors.New("resource ratio violates LimitRange")
					}
				}
			}
		}
	}
	total, err := effectivePodBudget(pod)
	if err != nil {
		return err
	}
	qs, err := s.Typed.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	for _, q := range qs.Items {
		if len(q.Spec.Scopes) > 0 || q.Spec.ScopeSelector != nil {
			return errors.New("scoped ResourceQuota requires a separately tested policy adapter")
		}
		for _, c := range append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...) {
			for _, k := range []corev1.ResourceName{"requests.cpu", "requests.memory", "limits.cpu", "limits.memory"} {
				if _, exists := q.Spec.Hard[k]; !exists {
					continue
				}
				ks := string(k)
				if strings.HasPrefix(ks, "requests.") {
					rk := corev1.ResourceName(strings.TrimPrefix(ks, "requests."))
					if _, ok := c.Resources.Requests[rk]; !ok {
						return errors.New("quota requires explicit or unambiguous default resource requests")
					}
				} else {
					rk := corev1.ResourceName(strings.TrimPrefix(ks, "limits."))
					if _, ok := c.Resources.Limits[rk]; !ok {
						return errors.New("quota requires explicit or unambiguous default resource limits")
					}
				}
			}
		}
		for k, need := range total {
			hard, ok := q.Spec.Hard[k]
			if !ok {
				continue
			}
			if need.Cmp(hard) > 0 {
				return errors.New("Notebook resource total exceeds namespace quota")
			}
			if enforceHeadroom {
				observed, ok := q.Status.Hard[k]
				if !ok || observed.Cmp(hard) != 0 {
					return errors.New("quota policy has not converged")
				}
				used := q.Status.Used[k]
				used.Add(need)
				if used.Cmp(hard) > 0 {
					return errors.New("insufficient ResourceQuota headroom after stop; Notebook remains stopped")
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, v := range pod.Volumes {
		if seen[v.Name] {
			return errors.New("duplicate volume name")
		}
		seen[v.Name] = true
		if v.PersistentVolumeClaim == nil {
			continue
		}
		p, err := s.Typed.CoreV1().PersistentVolumeClaims(ns).Get(ctx, v.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil {
			return safe(err)
		}
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.ClaimLost {
			return errors.New("referenced PVC is terminating or Lost")
		}
		e.add("PersistentVolumeClaim", ns, p.Name, struct {
			UID         types.UID
			Spec        corev1.PersistentVolumeClaimSpec
			Phase       corev1.PersistentVolumeClaimPhase
			Terminating bool
		}{p.UID, p.Spec, p.Status.Phase, p.DeletionTimestamp != nil})
		if p.Status.Phase != corev1.ClaimBound {
			if p.Spec.StorageClassName == nil {
				return errors.New("unbound PVC without explicit StorageClass")
			}
			sc, err := s.class(ctx, *p.Spec.StorageClassName, e)
			if err != nil {
				return err
			}
			if sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != "WaitForFirstConsumer" {
				return errors.New("PVC is not bound and is not waiting for first consumer")
			}
		} else {
			if p.Spec.VolumeName == "" {
				return errors.New("bound PVC lacks volume identity")
			}
			pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, p.Spec.VolumeName, metav1.GetOptions{})
			if err != nil {
				return safe(err)
			}
			r := pv.Spec.ClaimRef
			if r == nil || r.UID != p.UID || r.Namespace != ns || r.Name != p.Name || pv.DeletionTimestamp != nil {
				return errors.New("PVC/PV binding identity mismatch")
			}
			e.add("PersistentVolume", "", pv.Name, struct {
				UID         types.UID
				Spec        corev1.PersistentVolumeSpec
				Phase       corev1.PersistentVolumePhase
				Terminating bool
			}{pv.UID, pv.Spec, pv.Status.Phase, pv.DeletionTimestamp != nil})
			if pv.Spec.NodeAffinity != nil {
				return errors.New("mounting topology-constrained PVs requires a tested scheduling adapter")
			}
		}
		for _, c := range append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...) {
			for _, m := range c.VolumeMounts {
				if m.Name == v.Name && hasMode(p.Spec.AccessModes, corev1.ReadOnlyMany) && !m.ReadOnly && !v.PersistentVolumeClaim.ReadOnly {
					return errors.New("read-only claim requires a read-only mount")
				}
			}
		}
		if hasMode(p.Spec.AccessModes, corev1.ReadWriteOncePod) {
			pods, err := s.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return safe(err)
			}
			for _, other := range pods.Items {
				for _, ov := range other.Spec.Volumes {
					if ov.PersistentVolumeClaim != nil && ov.PersistentVolumeClaim.ClaimName == p.Name {
						return errors.New("ReadWriteOncePod claim already has a consumer; stop it before planning")
					}
				}
			}
		}
	}
	for _, c := range pod.Containers {
		for _, m := range c.VolumeMounts {
			if !seen[m.Name] {
				return errors.New("container mount references missing volume")
			}
		}
		b, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			return errors.New("invalid container resources")
		}
		res, _, _ := unstructured.NestedMap(b, "resources")
		if err = s.gpuNodes(ctx, spec, res, e); err != nil {
			return err
		}
	}
	return nil
}
func hasMode(xs []corev1.PersistentVolumeAccessMode, x corev1.PersistentVolumeAccessMode) bool {
	for _, m := range xs {
		if m == x {
			return true
		}
	}
	return false
}

func controlledBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}
func (s *Service) notebookChildren(ctx context.Context, n *unstructured.Unstructured) ([]appsv1.StatefulSet, []corev1.Pod, error) {
	sets, err := s.Typed.AppsV1().StatefulSets(n.GetNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, safe(err)
	}
	pods, err := s.Typed.CoreV1().Pods(n.GetNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, safe(err)
	}
	owned, children := notebookChildrenFrom(n, sets.Items, pods.Items)
	return owned, children, nil
}

// notebookChildrenFrom uses UID ownership, never names/labels alone. Inventory
// supplies its bulk lists; mutation observation still fetches live namespace lists.
func notebookChildrenFrom(n *unstructured.Unstructured, sets []appsv1.StatefulSet, pods []corev1.Pod) ([]appsv1.StatefulSet, []corev1.Pod) {
	owned := []appsv1.StatefulSet{}
	uids := map[types.UID]bool{n.GetUID(): true}
	for _, st := range sets {
		if st.Namespace == n.GetNamespace() && controlledBy(st.OwnerReferences, n.GetUID()) {
			owned = append(owned, st)
			uids[st.UID] = true
		}
	}
	children := []corev1.Pod{}
	for _, p := range pods {
		if p.Namespace != n.GetNamespace() {
			continue
		}
		for uid := range uids {
			if controlledBy(p.OwnerReferences, uid) {
				children = append(children, p)
				break
			}
		}
	}
	return owned, children
}
func (s *Service) notebookObserved(ctx context.Context, n *unstructured.Unstructured, stop bool, ops []patchOp) (bool, error) {
	sets, pods, err := s.notebookChildren(ctx, n)
	if err != nil {
		return false, err
	}
	return notebookObservedFrom(n, stop, ops, sets, pods)
}

func notebookObservedFrom(n *unstructured.Unstructured, stop bool, ops []patchOp, sets []appsv1.StatefulSet, pods []corev1.Pod) (bool, error) {
	if len(sets) != 1 {
		return false, nil
	}
	st := sets[0]
	if st.DeletionTimestamp != nil || st.Status.ObservedGeneration < st.Generation {
		return false, nil
	}
	if stop {
		if st.Spec.Replicas == nil || *st.Spec.Replicas != 0 || st.Status.Replicas != 0 || len(pods) != 0 {
			return false, nil
		}
	} else {
		if st.Spec.Replicas == nil || *st.Spec.Replicas != 1 || st.Status.ReadyReplicas != 1 || st.Status.Replicas != 1 || st.Status.CurrentReplicas != 1 || st.Status.UpdatedReplicas != 1 || st.Status.CurrentRevision == "" || st.Status.CurrentRevision != st.Status.UpdateRevision || len(pods) != 1 || pods[0].DeletionTimestamp != nil || pods[0].Labels[appsv1.StatefulSetRevisionLabel] != st.Status.UpdateRevision {
			return false, nil
		}
		ready := false
		for _, c := range pods[0].Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return false, nil
		}
		want, _, _ := unstructured.NestedSlice(n.Object, "spec", "template", "spec", "containers")
		for _, raw := range want {
			c, ok := raw.(map[string]any)
			if !ok {
				return false, errors.New("unsupported container shape")
			}
			found := false
			for _, got := range pods[0].Status.ContainerStatuses {
				if got.Name == c["name"] && got.Ready {
					found = true
				}
			}
			if !found {
				return false, nil
			}
		}
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
	if err != nil {
		return false, errors.New("cannot inspect generated workload")
	}
	if !templateMatches(obj, n, ops) {
		return false, nil
	}
	if !stop {
		p, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pods[0])
		if err != nil {
			return false, errors.New("cannot inspect observed Pod")
		}
		podSpec, _, _ := unstructured.NestedMap(p, "spec")
		wrapped := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": podSpec}}}
		if !templateMatches(wrapped, n, ops) {
			return false, nil
		}
	}
	return true, nil
}
func templateMatches(obj map[string]any, n *unstructured.Unstructured, ops []patchOp) bool {
	// Resolve containers and volumes by NAME because the controller may reorder
	// or inject entries. Compare only explicitly edited fields.
	for _, op := range ops {
		if !strings.HasPrefix(op.Path, "/spec/template/spec/") {
			continue
		}
		p := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
		if len(p) > 4 && (p[3] == "containers" || p[3] == "volumes") {
			source, _, _ := unstructured.NestedSlice(n.Object, p[:4]...)
			i, err := strconv.Atoi(p[4])
			if err != nil || i < 0 || i >= len(source) {
				return false
			}
			desired, ok := source[i].(map[string]any)
			if !ok {
				return false
			}
			desiredName, ok := desired["name"].(string)
			if !ok {
				return false
			}
			target, _, _ := unstructured.NestedSlice(obj, p[:4]...)
			idx := -1
			for j, v := range target {
				m, ok := v.(map[string]any)
				if !ok {
					return false
				}
				if m["name"] == desiredName {
					idx = j
				}
			}
			if idx < 0 {
				return false
			}
			p[4] = strconv.Itoa(idx)
		}
		got, exists := valueAt(obj, p)
		if op.Op == "remove" {
			if exists {
				return false
			}
		} else if !exists && !zeroJSON(op.Value) || exists && !matchesJSON(op.Value, got) {
			return false
		}
	}
	return true
}
func zeroJSON(v any) bool {
	switch m := v.(type) {
	case nil:
		return true
	case bool:
		return !m
	case string:
		return m == ""
	case map[string]any:
		return len(m) == 0
	case []any:
		return len(m) == 0
	}
	return false
}

// Kubernetes omits false/empty defaults and admission may add independent
// mounts/sidecars. Match explicit fields, including named list entries, without
// comparing controller defaults such as workingDir, NB_PREFIX, ports or fsGroup.
func matchesJSON(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			actual, exists := g[k]
			if !exists {
				if zeroJSON(v) {
					continue
				}
				return false
			}
			if !matchesJSON(v, actual) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok {
			return false
		}
		for _, v := range w {
			m, ok := v.(map[string]any)
			if !ok {
				if digest(w) != digest(g) {
					return false
				}
				continue
			}
			n, ok := m["name"].(string)
			if !ok {
				if digest(w) != digest(g) {
					return false
				}
				continue
			}
			found := false
			for _, a := range g {
				gm, ok := a.(map[string]any)
				if ok && gm["name"] == n && matchesJSON(m, gm) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return digest(want) == digest(got)
	}
}
func valueAt(v any, parts []string) (any, bool) {
	for _, p := range parts {
		p = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		switch m := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = m[p]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(m) {
				return nil, false
			}
			v = m[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func (s *Service) executeNotebook(ctx context.Context, w *work, cp func(string) error) error {
	a := w.prepared.Action
	n := w.notebook
	fresh := func() (*unstructured.Unstructured, error) {
		cur, err := s.Dynamic.Resource(Notebooks).Namespace(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			return nil, safe(err)
		}
		if cur.GetUID() != n.GetUID() || cur.GetDeletionTimestamp() != nil {
			return nil, ErrConflict
		}
		return cur, nil
	}
	observe := func(stop bool, ops []patchOp) error {
		return s.wait(ctx, func() (bool, error) {
			cur, err := fresh()
			if err != nil {
				return false, err
			}
			if notebookFingerprint(cur) != notebookFingerprint(n) {
				return false, ErrConflict
			}
			_, stopped := cur.GetAnnotations()[stoppedKey]
			if stopped != stop {
				return false, ErrConflict
			}
			return s.notebookObserved(ctx, cur, stop, ops)
		})
	}
	if a.Kind == "notebook.start" || a.Kind == "notebook.stop" {
		if len(w.ops) > 0 {
			if err := checkpoint(cp, a.Kind); err != nil {
				return err
			}
			var err error
			n, err = s.patchNotebook(ctx, n, w.ops, false)
			if err != nil {
				return err
			}
		}
		return observe(a.Kind == "notebook.stop", nil)
	}
	_, wasStopped := n.GetAnnotations()[stoppedKey]
	if !wasStopped {
		if err := checkpoint(cp, "stop-notebook-before-edit"); err != nil {
			return err
		}
		var err error
		n, err = s.patchNotebook(ctx, n, stopOps(n, true), false)
		if err != nil {
			return err
		}
	}
	if err := observe(true, nil); err != nil {
		return fmt.Errorf("Notebook left stop-requested before edit: %w", err)
	}
	// Only status and the operation's lifecycle annotations may have changed.
	cur, err := fresh()
	if err != nil {
		return err
	}
	if notebookFingerprint(cur) != w.ev.Target {
		return fmt.Errorf("Notebook left stopped: %w", ErrConflict)
	}
	e := s.newEvidence()
	if err = s.scope(ctx, a.Namespace, &e); err != nil {
		return err
	}
	ops, err := s.notebookChanges(ctx, a, cur, &e)
	if err != nil {
		return err
	}
	e.Target = w.ev.Target
	if string(e.bytes()) != string(w.ev.bytes()) {
		return fmt.Errorf("Notebook left stopped: %w", ErrConflict)
	}
	approvedDependencies := e
	if _, stopped := cur.GetAnnotations()[stoppedKey]; !stopped {
		return ErrConflict
	}
	if _, err = s.patchNotebook(ctx, cur, ops, true); err != nil {
		return err
	}
	if err = checkpoint(cp, "edit-stopped-notebook"); err != nil {
		return err
	}
	n, err = s.patchNotebook(ctx, cur, ops, false)
	if err != nil {
		return err
	}
	if err = observe(true, ops); err != nil {
		return fmt.Errorf("Notebook left stopped after edit: %w", err)
	}
	if a.Restart && !wasStopped {
		cur, err = fresh()
		if err != nil {
			return err
		}
		if notebookFingerprint(cur) != notebookFingerprint(n) {
			return ErrConflict
		}
		e = s.newEvidence()
		if err = s.scope(ctx, a.Namespace, &e); err != nil {
			return err
		}
		if _, err = s.notebookChanges(ctx, model.Action{Kind: "notebook.start", Namespace: a.Namespace, Name: a.Name}, cur, &e); err != nil {
			return err
		}
		e.Target = approvedDependencies.Target
		if string(e.bytes()) != string(approvedDependencies.bytes()) {
			return fmt.Errorf("Notebook left stopped: restart dependencies changed: %w", ErrConflict)
		}
		if err = checkpoint(cp, "restart-edited-notebook"); err != nil {
			return err
		}
		n, err = s.patchNotebook(ctx, cur, stopOps(cur, false), false)
		if err != nil {
			return err
		}
		return observe(false, ops)
	}
	return nil
}
