package kube

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"kubeflow-admin/internal/model"
)

func (s *Service) class(ctx context.Context, n string, e *evidence) (*storagev1.StorageClass, error) {
	if n == "" || !has(s.Config.Storage.AllowedStorageClasses, n) {
		return nil, errors.New("explicit approved StorageClass required")
	}
	c, err := s.Typed.StorageV1().StorageClasses().Get(ctx, n, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if c.DeletionTimestamp != nil {
		return nil, errors.New("StorageClass is terminating")
	}
	e.add("StorageClass", "", n, c)
	return c, nil
}
func accessMode(v string) (corev1.PersistentVolumeAccessMode, error) {
	switch corev1.PersistentVolumeAccessMode(v) {
	case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadOnlyMany, corev1.ReadWriteOncePod:
		return corev1.PersistentVolumeAccessMode(v), nil
	}
	return "", errors.New("explicit supported access mode required")
}
func compatible(pv *corev1.PersistentVolume, c *corev1.PersistentVolumeClaim) error {
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return errors.New("existing PV reservation requires Retain reclaim policy")
	}
	if pv.DeletionTimestamp != nil || pv.Spec.ClaimRef != nil || pv.Status.Phase != corev1.VolumeAvailable {
		return errors.New("PV must be Available, unreserved, and nonterminating; Released volumes require external data review")
	}
	if pv.Spec.NodeAffinity != nil {
		return errors.New("topology-constrained static imports are unsupported")
	}
	if pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return errors.New("block-mode volumes are unsupported")
	}
	if c.Spec.StorageClassName == nil || pv.Spec.StorageClassName != *c.Spec.StorageClassName {
		return errors.New("PV and PVC class mismatch")
	}
	cap := pv.Spec.Capacity.Storage()
	if cap.Cmp(*c.Spec.Resources.Requests.Storage()) < 0 {
		return errors.New("insufficient PV capacity")
	}
	for _, m := range c.Spec.AccessModes {
		found := false
		for _, p := range pv.Spec.AccessModes {
			if p == m {
				found = true
			}
		}
		if !found {
			return errors.New("PV access mode mismatch")
		}
	}
	if pv.Spec.CSI == nil && pv.Spec.NFS == nil {
		return errors.New("only approved network filesystem volumes can be reserved")
	}
	return nil
}
func (s *Service) storageQuota(ctx context.Context, ns string, size resource.Quantity, newClaim bool) error {
	qs, err := s.Typed.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	for _, q := range qs.Items {
		for key, hard := range q.Status.Hard {
			var delta resource.Quantity
			ks := string(key)
			switch {
			case ks == "persistentvolumeclaims" || strings.HasSuffix(ks, ".storageclass.storage.k8s.io/persistentvolumeclaims"):
				if !newClaim {
					continue
				}
				delta = resource.MustParse("1")
			case ks == "requests.storage" || strings.HasSuffix(ks, ".storageclass.storage.k8s.io/requests.storage"):
				delta = size
			default:
				continue
			}
			used := q.Status.Used[key]
			used.Add(delta)
			if used.Cmp(hard) > 0 {
				return errors.New("storage request exceeds observed ResourceQuota headroom")
			}
		}
	}
	return nil
}
func (s *Service) prepareStorage(ctx context.Context, a model.Action, w *work) error {
	size, err := quantity(a.Size)
	if err != nil {
		return err
	}
	if a.Kind == "pvc.expand" {
		if !s.Config.Features.PVCExpansion {
			return errors.New("PVC expansion is disabled")
		}
		p, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			return safe(err)
		}
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.ClaimBound || p.Spec.StorageClassName == nil || p.Spec.VolumeName == "" {
			return errors.New("expansion requires a bound nonterminating PVC with explicit class")
		}
		if size.Cmp(*p.Spec.Resources.Requests.Storage()) <= 0 {
			return errors.New("PVC expansion must increase requested size")
		}
		c, err := s.class(ctx, *p.Spec.StorageClassName, &w.ev)
		if err != nil {
			return err
		}
		if c.AllowVolumeExpansion == nil || !*c.AllowVolumeExpansion {
			return errors.New("StorageClass does not allow expansion")
		}
		pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, p.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return safe(err)
		}
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != c.Provisioner || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != p.UID || pv.Spec.ClaimRef.Name != p.Name || pv.Spec.ClaimRef.Namespace != p.Namespace {
			return errors.New("expansion requires a verified CSI binding and matching driver")
		}
		w.ev.add("PersistentVolume", "", pv.Name, pv)
		delta := size.DeepCopy()
		delta.Sub(*p.Spec.Resources.Requests.Storage())
		if err = s.storageQuota(ctx, a.Namespace, delta, false); err != nil {
			return err
		}
		w.pvc = p
		w.prepared.TargetUID = string(p.UID)
		w.prepared.ResourceVersion = p.ResourceVersion
		w.ev.Target = digest(p.Spec)
		w.ops = []patchOp{{"replace", "/spec/resources/requests/storage", size.String()}}
		w.prepared.Warnings = append(w.prepared.Warnings, "CSI/controller and filesystem resize must complete; driver/backend support requires deployment verification.")
		return nil
	}
	mode, err := accessMode(a.AccessMode)
	if err != nil {
		return err
	}
	if _, err = s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrConflict
		}
		return safe(err)
	}
	fs := corev1.PersistentVolumeFilesystem
	class := a.StorageClass
	p := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: a.Name, Namespace: a.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{mode}, VolumeMode: &fs, StorageClassName: &class, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}}}}
	w.pvc = p
	if err = s.storageQuota(ctx, a.Namespace, size, true); err != nil {
		return err
	}
	if a.Kind == "pvc.create.dynamic" {
		c, err := s.class(ctx, a.StorageClass, &w.ev)
		if err != nil {
			return err
		}
		if c.Provisioner == "kubernetes.io/no-provisioner" {
			return errors.New("class has no dynamic provisioner")
		}
		w.delayed = c.VolumeBindingMode != nil && *c.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
		if w.delayed {
			w.prepared.Warnings = append(w.prepared.Warnings, "WaitForFirstConsumer: Pending is expected until a consuming Pod is scheduled.")
		}
		if c.ReclaimPolicy != nil {
			w.prepared.Warnings = append(w.prepared.Warnings, "StorageClass reclaim policy: "+string(*c.ReclaimPolicy))
		}
		return nil
	}
	if !s.Config.Features.StaticStorage {
		return errors.New("static storage is disabled")
	}
	if err = name(a.PVName); err != nil {
		return err
	}
	p.Spec.VolumeName = a.PVName
	if a.Kind == "pv.reserve-and-claim" {
		if a.StorageClass != "" {
			if _, err = s.class(ctx, a.StorageClass, &w.ev); err != nil {
				return err
			}
		}
		v, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, a.PVName, metav1.GetOptions{})
		if err != nil {
			return safe(err)
		}
		if err = compatible(v, p); err != nil {
			return err
		}
		if err = s.approvedExistingVolume(v); err != nil {
			return err
		}
		w.pv = v
		w.prepared.TargetUID = string(v.UID)
		w.prepared.ResourceVersion = v.ResourceVersion
		w.ev.Target = digest(v.Spec)
		w.ops = []patchOp{{"add", "/spec/claimRef", map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": a.Name, "namespace": a.Namespace}}}
		return nil
	}
	if a.Kind != "storage.create.static" {
		return errors.New("unsupported storage action")
	}
	if _, err = s.Typed.CoreV1().PersistentVolumes().Get(ctx, a.PVName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrConflict
		}
		return safe(err)
	}
	v := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: a.PVName}, Spec: corev1.PersistentVolumeSpec{Capacity: corev1.ResourceList{corev1.ResourceStorage: size}, AccessModes: p.Spec.AccessModes, VolumeMode: &fs, StorageClassName: a.StorageClass, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: a.Namespace, Name: a.Name}}}
	found := false
	for _, t := range s.Config.Storage.NFSTemplates {
		if t.Name != a.Template {
			continue
		}
		if found {
			return errors.New("ambiguous template name")
		}
		found = true
		if t.StorageClass != a.StorageClass || !has(t.AccessModes, a.AccessMode) {
			return errors.New("template class/access mismatch")
		}
		sub := a.Subdirectory
		if sub == "" || sub == "." || sub == ".." || path.IsAbs(sub) || path.Clean(sub) != sub || strings.Contains(sub, "\\") || strings.HasPrefix(sub, "../") {
			return errors.New("canonical nonempty relative NFS subdirectory required")
		}
		if t.Server == "" || !path.IsAbs(t.BasePath) || path.Clean(t.BasePath) != t.BasePath {
			return errors.New("invalid approved NFS template")
		}
		joined := path.Join(t.BasePath, sub)
		base := strings.TrimSuffix(path.Clean(t.BasePath), "/")
		if !strings.HasPrefix(joined, base+"/") || joined == base {
			return errors.New("NFS path must remain strictly beneath approved base")
		}
		v.Spec.NFS = &corev1.NFSVolumeSource{Server: t.Server, Path: joined}
		v.Spec.MountOptions = append([]string(nil), t.MountOptions...)
	}
	// CSI imports use an exact reviewed handle and fixed attributes. No user
	// supplied volume handles or arbitrary driver attributes are accepted.
	for _, t := range s.Config.Storage.CSITemplates {
		if t.Name != a.Template {
			continue
		}
		if found {
			return errors.New("ambiguous template name")
		}
		found = true
		if t.Driver == "" || t.VolumeHandle == "" || a.Subdirectory != "" || t.StorageClass != a.StorageClass || !has(t.AccessModes, a.AccessMode) {
			return errors.New("invalid or incompatible fixed CSI template")
		}
		attrs := map[string]string{}
		for k, v := range t.VolumeAttributes {
			attrs[k] = v
		}
		v.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: t.Driver, VolumeHandle: t.VolumeHandle, VolumeAttributes: attrs}
	}
	if !found {
		return errors.New("approved storage template required")
	}
	if a.StorageClass != "" {
		c, err := s.class(ctx, a.StorageClass, &w.ev)
		if err != nil {
			return err
		}
		if v.Spec.CSI != nil && v.Spec.CSI.Driver != c.Provisioner {
			return errors.New("CSI template driver does not match class")
		}
	}
	all, err := s.Typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	for _, other := range all.Items {
		if sameBackend(v, &other) {
			return errors.New("backing storage is already represented by a PV")
		}
	}
	w.ev.add("PersistentVolumeList", "", "", all.Items)
	w.pv = v
	w.prepared.Warnings = append(w.prepared.Warnings, "Registers existing storage only; does not create directories, set permissions, allocate backend space, or move files.")
	return nil
}
func pathWithin(base, candidate string) bool {
	base = path.Clean(base)
	candidate = path.Clean(candidate)
	return base == candidate || base == "/" && path.IsAbs(candidate) || strings.HasPrefix(candidate, strings.TrimSuffix(base, "/")+"/")
}
func sameBackend(a, b *corev1.PersistentVolume) bool {
	if a.Spec.NFS != nil && b.Spec.NFS != nil {
		return a.Spec.NFS.Server == b.Spec.NFS.Server && (pathWithin(a.Spec.NFS.Path, b.Spec.NFS.Path) || pathWithin(b.Spec.NFS.Path, a.Spec.NFS.Path))
	}
	if a.Spec.CSI != nil && b.Spec.CSI != nil {
		return a.Spec.CSI.Driver == b.Spec.CSI.Driver && a.Spec.CSI.VolumeHandle == b.Spec.CSI.VolumeHandle
	}
	return false
}
func (s *Service) approvedExistingVolume(v *corev1.PersistentVolume) error {
	if v.Spec.NFS != nil {
		for _, t := range s.Config.Storage.NFSTemplates {
			if t.Server == v.Spec.NFS.Server && !v.Spec.NFS.ReadOnly && t.StorageClass == v.Spec.StorageClassName && path.IsAbs(t.BasePath) && path.Clean(v.Spec.NFS.Path) == v.Spec.NFS.Path && v.Spec.NFS.Path != path.Clean(t.BasePath) && pathWithin(t.BasePath, v.Spec.NFS.Path) && equalStrings(v.Spec.MountOptions, t.MountOptions) && modesApproved(v.Spec.AccessModes, t.AccessModes) {
				return nil
			}
		}
	}
	if v.Spec.CSI != nil {
		for _, t := range s.Config.Storage.CSITemplates {
			if t.Driver == v.Spec.CSI.Driver && t.VolumeHandle == v.Spec.CSI.VolumeHandle && t.StorageClass == v.Spec.StorageClassName && same(t.VolumeAttributes, v.Spec.CSI.VolumeAttributes) && !v.Spec.CSI.ReadOnly && v.Spec.CSI.FSType == "" && v.Spec.CSI.ControllerPublishSecretRef == nil && v.Spec.CSI.NodeStageSecretRef == nil && v.Spec.CSI.NodePublishSecretRef == nil && v.Spec.CSI.ControllerExpandSecretRef == nil && v.Spec.CSI.NodeExpandSecretRef == nil && len(v.Spec.MountOptions) == 0 && modesApproved(v.Spec.AccessModes, t.AccessModes) {
				return nil
			}
		}
	}
	return errors.New("PV backing source is not in an approved template")
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func modesApproved(modes []corev1.PersistentVolumeAccessMode, approved []string) bool {
	for _, m := range modes {
		if !has(approved, string(m)) {
			return false
		}
	}
	return len(modes) > 0
}
func (s *Service) executeStorageOrProfile(ctx context.Context, w *work, cp func(string) error) error {
	a := w.prepared.Action
	switch a.Kind {
	case "profile.create":
		if err := checkpoint(cp, "create-profile"); err != nil {
			return err
		}
		p, err := s.Dynamic.Resource(Profiles).Create(ctx, w.profile, metav1.CreateOptions{})
		if err != nil {
			return safe(err)
		}
		return s.wait(ctx, func() (bool, error) {
			cur, err := s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{})
			if err != nil {
				return false, safe(err)
			}
			ownerName, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "name")
			ownerKind, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "kind")
			if cur.GetUID() != p.GetUID() || cur.GetDeletionTimestamp() != nil || ownerName != a.Owner || ownerKind != "User" {
				return false, ErrConflict
			}
			ns, err := s.Typed.CoreV1().Namespaces().Get(ctx, a.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				return false, safe(err)
			}
			if ns.Status.Phase != corev1.NamespaceActive || ns.DeletionTimestamp != nil {
				return false, nil
			}
			if !controlledBy(ns.OwnerReferences, p.GetUID()) || ns.Annotations["owner"] != a.Owner {
				return false, ErrConflict
			}
			rb, err := s.Typed.RbacV1().RoleBindings(a.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, safe(err)
			}
			access := false
			for _, r := range rb.Items {
				for _, sub := range r.Subjects {
					if sub.Kind == "User" && sub.Name == a.Owner && sub.APIGroup == "rbac.authorization.k8s.io" && r.RoleRef.Kind == "ClusterRole" && r.RoleRef.Name == "kubeflow-admin" && r.RoleRef.APIGroup == "rbac.authorization.k8s.io" && controlledBy(r.OwnerReferences, p.GetUID()) && r.DeletionTimestamp == nil {
						access = true
					}
				}
			}
			if !access {
				return false, nil
			}
			return s.quotaObserved(ctx, a.Name, a.Quota, p.GetUID())
		})
	case "profile.quota.change":
		b, _ := json.Marshal(conditional(string(w.profile.GetUID()), w.profile.GetResourceVersion(), w.ops))
		if err := checkpoint(cp, "patch-profile-quota"); err != nil {
			return err
		}
		if _, err := s.Dynamic.Resource(Profiles).Patch(ctx, a.Name, types.JSONPatchType, b, metav1.PatchOptions{}); err != nil {
			return safe(err)
		}
		oldOwner, _, _ := unstructured.NestedString(w.profile.Object, "spec", "owner", "name")
		return s.wait(ctx, func() (bool, error) {
			cur, err := s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{})
			if err != nil {
				return false, safe(err)
			}
			o, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "name")
			if cur.GetUID() != w.profile.GetUID() || cur.GetDeletionTimestamp() != nil || o != oldOwner {
				return false, ErrConflict
			}
			e := s.newEvidence()
			if err = s.scope(ctx, a.Name, &e); err != nil {
				return false, err
			}
			return s.quotaObserved(ctx, a.Name, a.Quota, w.profile.GetUID())
		})
	case "pvc.expand":
		b, _ := json.Marshal(conditional(string(w.pvc.UID), w.pvc.ResourceVersion, w.ops))
		if err := checkpoint(cp, "expand-pvc"); err != nil {
			return err
		}
		p, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Patch(ctx, a.Name, types.JSONPatchType, b, metav1.PatchOptions{})
		if err != nil {
			return safe(err)
		}
		size, _ := quantity(a.Size)
		return s.wait(ctx, func() (bool, error) {
			cur, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
			if err != nil {
				return false, safe(err)
			}
			if cur.UID != p.UID {
				return false, ErrConflict
			}
			cap := cur.Status.Capacity.Storage()
			if cap.Cmp(size) < 0 {
				return false, nil
			}
			for _, c := range cur.Status.Conditions {
				if c.Status == corev1.ConditionTrue && (c.Type == corev1.PersistentVolumeClaimResizing || c.Type == corev1.PersistentVolumeClaimFileSystemResizePending) {
					return false, nil
				}
			}
			return true, nil
		})
	}
	var reserved *corev1.PersistentVolume
	if a.Kind == "storage.create.static" {
		if err := checkpoint(cp, "create-reserved-pv"); err != nil {
			return err
		}
		v, err := s.Typed.CoreV1().PersistentVolumes().Create(ctx, w.pv, metav1.CreateOptions{})
		if err != nil {
			return safe(err)
		}
		reserved = v
	}
	if a.Kind == "pv.reserve-and-claim" {
		b, _ := json.Marshal(conditional(string(w.pv.UID), w.pv.ResourceVersion, w.ops))
		if err := checkpoint(cp, "reserve-pv"); err != nil {
			return err
		}
		v, err := s.Typed.CoreV1().PersistentVolumes().Patch(ctx, w.pv.Name, types.JSONPatchType, b, metav1.PatchOptions{})
		if err != nil {
			return safe(err)
		}
		reserved = v
	}
	if reserved != nil {
		if err := s.checkEvidence(ctx, a.Namespace, w.ev); err != nil {
			return err
		}
		v, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, reserved.Name, metav1.GetOptions{})
		if err != nil {
			return safe(err)
		}
		r := v.Spec.ClaimRef
		if v.UID != reserved.UID || v.DeletionTimestamp != nil || r == nil || r.Name != a.Name || r.Namespace != a.Namespace || r.UID != "" {
			return ErrConflict
		}
	}
	if err := checkpoint(cp, "create-pvc"); err != nil {
		return err
	}
	p, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Create(ctx, w.pvc, metav1.CreateOptions{})
	if err != nil {
		return safe(err)
	}
	return s.wait(ctx, func() (bool, error) {
		cur, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if cur.UID != p.UID || cur.DeletionTimestamp != nil {
			return false, ErrConflict
		}
		if w.delayed && cur.Status.Phase == corev1.ClaimPending {
			return true, nil
		}
		if cur.Status.Phase != corev1.ClaimBound || cur.Spec.VolumeName == "" {
			return false, nil
		}
		v, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, cur.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if reserved != nil && v.UID != reserved.UID {
			return false, ErrConflict
		}
		r := v.Spec.ClaimRef
		if r == nil || r.UID != cur.UID || r.Namespace != cur.Namespace || r.Name != cur.Name {
			return false, errors.New("PVC/PV binding identity mismatch")
		}
		return v.Status.Phase == corev1.VolumeBound, nil
	})
}
func (s *Service) quotaObserved(ctx context.Context, ns string, desired map[string]string, uid types.UID) (bool, error) {
	if len(desired) == 0 {
		return true, nil
	}
	qs, err := s.Typed.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, safe(err)
	}
	for _, q := range qs.Items {
		if q.Name != "kf-resource-quota" || !controlledBy(q.OwnerReferences, uid) || q.DeletionTimestamp != nil {
			continue
		}
		ok := true
		for k, v := range desired {
			want, _ := quantity(v)
			got, exists := q.Spec.Hard[corev1.ResourceName(k)]
			if !exists || got.Cmp(want) != 0 {
				ok = false
			}
			observed, exists := q.Status.Hard[corev1.ResourceName(k)]
			if !exists || observed.Cmp(want) != 0 {
				ok = false
			}
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
