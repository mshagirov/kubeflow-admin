package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"kubeflow-admin/internal/model"
)

// Inventory intentionally projects summaries, not manifests or literal env,
// credentials, NFS server paths, event messages, or container log contents.
func (s *Service) Inventory(ctx context.Context) (model.Inventory, error) {
	out := model.Inventory{ObservedAt: time.Now().UTC(), Resources: []model.Resource{}}
	add := func(kind, ns, n, uid, state string, d map[string]string) {
		out.Resources = append(out.Resources, model.Resource{Kind: kind, Namespace: ns, Name: n, UID: uid, State: state, Details: d})
	}
	ps, err := s.Dynamic.Resource(Profiles).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, p := range ps.Items {
		o, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
		r := model.Resource{Kind: "Profile", Name: p.GetName(), UID: string(p.GetUID()), Owner: o}
		if s.namespacePolicy(p.GetName()) != nil {
			r.State = "Protected"
		}
		out.Resources = append(out.Resources, r)
	}
	ns, err := s.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, n := range ns.Items {
		details := map[string]string{}
		if !n.CreationTimestamp.IsZero() {
			details["createdAt"] = n.CreationTimestamp.UTC().Format(time.RFC3339)
		}
		add("Namespace", "", n.Name, string(n.UID), string(n.Status.Phase), details)
	}
	nb, err := s.Dynamic.Resource(Notebooks).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	// Fetch these once, not once (or twice) per notebook. The same observations
	// populate the tables and lifecycle summaries; mutation paths do not use them.
	pods, err := s.Typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	sts, err := s.Typed.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, n := range nb.Items {
		state := "Starting"
		if _, ok := n.GetAnnotations()[stoppedKey]; ok {
			state = "Stopping"
		}
		d := map[string]string{}
		cs, _, _ := unstructured.NestedSlice(n.Object, "spec", "template", "spec", "containers")
		for _, c := range cs {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cn, _ := m["name"].(string)
			im, _ := m["image"].(string)
			d["image."+cn] = im
		}
		sets, children := notebookChildrenFrom(&n, sts.Items, pods.Items)
		_, stopped := n.GetAnnotations()[stoppedKey]
		observed, observeErr := notebookObservedFrom(&n, stopped, nil, sets, children)
		if observeErr != nil {
			return out, observeErr
		}
		if observed {
			state = "Running / Ready"
			if stopped {
				state = "Stopped"
			}
		}
		add("Notebook", n.GetNamespace(), n.GetName(), string(n.GetUID()), state, d)
	}
	pv, err := s.Typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range pv.Items {
		cap := v.Spec.Capacity.Storage()
		d := map[string]string{"capacity": cap.String(), "storageClass": v.Spec.StorageClassName, "reclaimPolicy": string(v.Spec.PersistentVolumeReclaimPolicy), "filesystemUsage": "not available", "backendCapacity": "not available"}
		if v.Spec.ClaimRef != nil {
			d["claim"] = v.Spec.ClaimRef.Namespace + "/" + v.Spec.ClaimRef.Name
		}
		add("PersistentVolume", "", v.Name, string(v.UID), string(v.Status.Phase), d)
	}
	pvc, err := s.Typed.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range pvc.Items {
		req := v.Spec.Resources.Requests.Storage()
		cap := v.Status.Capacity.Storage()
		d := map[string]string{"requested": req.String(), "capacity": cap.String(), "volume": v.Spec.VolumeName, "filesystemUsage": "not available"}
		if v.Spec.StorageClassName != nil {
			d["storageClass"] = *v.Spec.StorageClassName
		}
		for _, c := range v.Status.Conditions {
			if c.Status == "True" {
				d["condition."+string(c.Type)] = c.Reason
			}
		}
		add("PersistentVolumeClaim", v.Namespace, v.Name, string(v.UID), string(v.Status.Phase), d)
	}
	sc, err := s.Typed.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range sc.Items {
		d := map[string]string{"provisioner": v.Provisioner, "approved": strconv.FormatBool(has(s.Config.Storage.AllowedStorageClasses, v.Name))}
		if v.VolumeBindingMode != nil {
			d["bindingMode"] = string(*v.VolumeBindingMode)
		}
		if v.ReclaimPolicy != nil {
			d["reclaimPolicy"] = string(*v.ReclaimPolicy)
		}
		add("StorageClass", "", v.Name, string(v.UID), "", d)
	}
	for _, p := range pods.Items {
		d := map[string]string{"node": p.Spec.NodeName, "liveUsage": "not available"}
		if owner := podControllerOwner(&p); owner != "" {
			d["owner"] = owner
		}
		ready, readyContainers, totalContainers := podReadiness(&p)
		d["ready"] = strconv.FormatBool(ready)
		d["containers"] = fmt.Sprintf("%d/%d ready", readyContainers, totalContainers)
		d["restarts"] = strconv.Itoa(podRestartTotal(&p))
		for key, value := range podResourceDetails(&p) {
			d[key] = value
		}
		for _, c := range p.Status.Conditions {
			if c.Status == "False" {
				d["condition."+string(c.Type)] = c.Reason
			}
		}
		for _, c := range p.Status.ContainerStatuses {
			d["restarts."+c.Name] = strconv.Itoa(int(c.RestartCount))
			if c.State.Waiting != nil {
				d["waiting."+c.Name] = c.State.Waiting.Reason
			}
			if c.State.Terminated != nil {
				d["terminated."+c.Name] = c.State.Terminated.Reason
			}
		}
		out.Resources = append(out.Resources, model.Resource{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: string(p.UID), Owner: d["owner"], State: string(p.Status.Phase), Details: d})
	}
	for _, v := range sts.Items {
		add("StatefulSet", v.Namespace, v.Name, string(v.UID), fmt.Sprintf("%d ready", v.Status.ReadyReplicas), nil)
	}
	qs, err := s.Typed.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range qs.Items {
		d := map[string]string{}
		for k, q := range v.Status.Hard {
			d["hard."+string(k)] = q.String()
		}
		for k, q := range v.Status.Used {
			d["used."+string(k)] = q.String()
		}
		add("ResourceQuota", v.Namespace, v.Name, string(v.UID), "", d)
	}
	ls, err := s.Typed.CoreV1().LimitRanges("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range ls.Items {
		add("LimitRange", v.Namespace, v.Name, string(v.UID), "", nil)
	}
	rb, err := s.Typed.RbacV1().RoleBindings("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range rb.Items {
		add("RoleBinding", v.Namespace, v.Name, string(v.UID), "", map[string]string{"role": v.RoleRef.Kind + "/" + v.RoleRef.Name})
	}
	ev, err := s.Typed.CoreV1().Events("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range ev.Items {
		add("Event", v.Namespace, v.Name, string(v.UID), v.Type, map[string]string{"reason": v.Reason, "object": v.InvolvedObject.Kind + "/" + v.InvolvedObject.Name})
	}
	nodes, err := s.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, v := range nodes.Items {
		d := map[string]string{}
		for k, q := range v.Status.Allocatable {
			if strings.Contains(string(k), "/") {
				d["allocatable."+string(k)] = q.String()
			}
		}
		roles := nodeRoles(&v)
		if len(roles) == 0 {
			d["roles"] = "<none>"
		} else {
			d["roles"] = strings.Join(roles, ",")
		}
		if !v.CreationTimestamp.IsZero() {
			d["createdAt"] = v.CreationTimestamp.UTC().Format(time.RFC3339)
		}
		d["version"] = v.Status.NodeInfo.KubeletVersion
		d["internalIP"] = nodeAddress(&v, corev1.NodeInternalIP)
		if d["internalIP"] == "" {
			d["internalIP"] = "<none>"
		}
		d["externalIP"] = nodeAddress(&v, corev1.NodeExternalIP)
		if d["externalIP"] == "" {
			d["externalIP"] = "<none>"
		}
		d["osImage"] = v.Status.NodeInfo.OSImage
		d["kernelVersion"] = v.Status.NodeInfo.KernelVersion
		d["containerRuntime"] = v.Status.NodeInfo.ContainerRuntimeVersion
		d["unschedulable"] = strconv.FormatBool(v.Spec.Unschedulable)
		d["controlPlane"] = strconv.FormatBool(isControlPlaneNode(&v))
		add("Node", "", v.Name, string(v.UID), nodeState(&v), d)
	}
	out.Warnings = []string{"Filesystem usage and storage-backend capacity are not available from Kubernetes inventory.", "Notebook CR dry-run does not validate controller-generated Pod admission or scheduling."}
	return out, nil
}

// nodeReady reports the NodeReady condition exactly; a missing condition is not
// readiness. nodeState mirrors `kubectl get nodes` for the inventory and the
// cordon preview.
func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeState(n *corev1.Node) string {
	state := "Unknown"
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		switch c.Status {
		case corev1.ConditionTrue:
			state = "Ready"
		case corev1.ConditionFalse:
			state = "NotReady"
		}
		break
	}
	if n.Spec.Unschedulable {
		state += ",SchedulingDisabled"
	}
	return state
}

func nodeDesiredState(n *corev1.Node, unschedulable bool) string {
	c := n.DeepCopy()
	c.Spec.Unschedulable = unschedulable
	return nodeState(c)
}

// nodeRoles extracts sorted role names from the conventional role labels.
// Empty label values are still roles: `kubectl` renders the label key itself.
func nodeRoles(n *corev1.Node) []string {
	seen := map[string]bool{}
	if role := n.Labels["kubernetes.io/role"]; role != "" {
		seen[role] = true
	}
	for key := range n.Labels {
		role, ok := strings.CutPrefix(key, "node-role.kubernetes.io/")
		if ok && role != "" {
			seen[role] = true
		}
	}
	roles := make([]string, 0, len(seen))
	for role := range seen {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

func isControlPlaneNode(n *corev1.Node) bool {
	for _, role := range nodeRoles(n) {
		if role == "control-plane" || role == "master" {
			return true
		}
	}
	return false
}

func nodeAddress(n *corev1.Node, kind corev1.NodeAddressType) string {
	for _, a := range n.Status.Addresses {
		if a.Type == kind {
			return a.Address
		}
	}
	return ""
}

func podControllerOwner(p *corev1.Pod) string {
	c := metav1.GetControllerOf(p)
	if c == nil {
		return ""
	}
	return c.Kind + "/" + c.Name
}

func podReadiness(p *corev1.Pod) (bool, int, int) {
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	readyContainers := 0
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			readyContainers++
		}
	}
	return ready, readyContainers, len(p.Spec.Containers)
}

func podRestartTotal(p *corev1.Pod) int {
	total := 0
	for _, c := range p.Status.ContainerStatuses {
		total += int(c.RestartCount)
	}
	return total
}

// podResourceDetails reports declared requests/limits, never live usage. It
// sums regular containers, applies ordinary init maxima and adds Pod overhead.
// It is a table summary, not an admission or scheduling calculation.
func podResourceDetails(p *corev1.Pod) map[string]string {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	for _, c := range p.Spec.Containers {
		for name, q := range c.Resources.Requests {
			cur := requests[name]
			cur.Add(q)
			requests[name] = cur
		}
		for name, q := range c.Resources.Limits {
			cur := limits[name]
			cur.Add(q)
			limits[name] = cur
		}
	}
	for _, c := range p.Spec.InitContainers {
		for name, q := range c.Resources.Requests {
			if cur, ok := requests[name]; !ok || q.Cmp(cur) > 0 {
				requests[name] = q.DeepCopy()
			}
		}
		for name, q := range c.Resources.Limits {
			if cur, ok := limits[name]; !ok || q.Cmp(cur) > 0 {
				limits[name] = q.DeepCopy()
			}
		}
	}
	for name, q := range p.Spec.Overhead {
		for _, target := range []corev1.ResourceList{requests, limits} {
			cur := target[name]
			cur.Add(q)
			target[name] = cur
		}
	}
	out := map[string]string{}
	for _, item := range []struct {
		key   string
		name  corev1.ResourceName
		table corev1.ResourceList
	}{
		{"requested.cpu", corev1.ResourceCPU, requests},
		{"requested.memory", corev1.ResourceMemory, requests},
		{"limit.cpu", corev1.ResourceCPU, limits},
		{"limit.memory", corev1.ResourceMemory, limits},
	} {
		q := item.table[item.name]
		if q.Sign() > 0 {
			out[item.key] = q.String()
		}
	}
	return out
}
