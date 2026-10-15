package kube

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
		add("Namespace", "", n.Name, string(n.UID), string(n.Status.Phase), nil)
	}
	nb, err := s.Dynamic.Resource(Notebooks).Namespace("").List(ctx, metav1.ListOptions{})
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
		observed, observeErr := s.notebookObserved(ctx, &n, false, nil)
		if observeErr != nil {
			return out, observeErr
		}
		if observed {
			state = "Running / Ready"
		} else if _, stop := n.GetAnnotations()[stoppedKey]; stop {
			stopped, observeErr := s.notebookObserved(ctx, &n, true, nil)
			if observeErr != nil {
				return out, observeErr
			}
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
	pods, err := s.Typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
	}
	for _, p := range pods.Items {
		d := map[string]string{"node": p.Spec.NodeName}
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
		add("Pod", p.Namespace, p.Name, string(p.UID), string(p.Status.Phase), d)
	}
	sts, err := s.Typed.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, safe(err)
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
		add("Node", "", v.Name, string(v.UID), "", d)
	}
	out.Warnings = []string{"Filesystem usage and storage-backend capacity are not available from Kubernetes inventory.", "Notebook CR dry-run does not validate controller-generated Pod admission or scheduling."}
	return out, nil
}
