package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"kubeflow-admin/internal/model"
)

// MaxPodDefaultCleanup bounds the reviewed PodDefault dependency set of one
// PVC deletion. A larger set is refused explicitly instead of being truncated:
// silently dropping a reviewed dependency would leave a PodDefault behind and
// silently adding one would mutate an object the administrator never reviewed.
const MaxPodDefaultCleanup = 32

// reviewedPodDefaultIdentitiesField is the single immutable acceptance change
// that snapshots the exact reviewed dependency identities. It is deliberately
// independent of backend presentation wording and contains names, namespaces,
// UIDs and resourceVersions only.
const reviewedPodDefaultIdentitiesField = "Reviewed PodDefault identities"

// podDefaultLockTarget returns the canonical secondary lock for one PodDefault.
// A PodDefault is namespaced; its namespace is always the namespace of the PVC
// workflow that reviews it, never a browser-supplied value.
func podDefaultLockTarget(namespace, name string) (string, bool) {
	if !validDNS1123Label(namespace) || !validTargetName(name) {
		return "", false
	}
	return "poddefault:" + namespace + "/" + name, true
}

// podDefaultCreationTarget derives the optional PodDefault lock from a PVC
// creation action. A nil request creates no lock. A malformed request is
// refused before any claim or lock exists.
func podDefaultCreationTarget(a model.Action) (string, error) {
	if a.PodDefault == nil {
		return "", nil
	}
	if !supportsPodDefaultCreation(a.Kind) {
		return "", ErrInvalidTarget
	}
	target, ok := podDefaultLockTarget(a.Namespace, a.PodDefault.Name)
	if !ok {
		return "", ErrInvalidTarget
	}
	return target, nil
}

// supportsPodDefaultCreation reports the PVC creation kinds that may carry the
// optional PodDefault request. The legacy combined static and reservation
// actions keep their old shape unchanged and never acquire an undocumented
// PodDefault substep.
func supportsPodDefaultCreation(kind string) bool {
	switch kind {
	case "pvc.create.dynamic", "pvc.create.bound":
		return true
	}
	return false
}

// validatePodDefaultContract enforces the typed fields before admission:
// a PodDefault request is accepted only on a PVC creation kind, a cleanup
// acknowledgement only on the PVC deletion kind, and a cleanup acknowledgement
// must carry a complete safe reviewed dependency list.
func validatePodDefaultContract(p model.Prepared) error {
	a := p.Action
	if a.PodDefault != nil {
		if _, err := podDefaultCreationTarget(a); err != nil {
			return err
		}
	}
	if a.Kind != pvcDeleteKind {
		if a.ConfirmPodDefaultCleanup || a.ConfirmPVReclaim {
			return ErrInvalidTarget
		}
		return nil
	}
	list, canonical, noncanonical, err := scanPodDefaultEvidence(p.Data)
	if err != nil || noncanonical {
		// A cleanup-shaped alias, wrapper or case variant is refused even when
		// the acknowledgement is false. It is never reinterpreted as a true
		// legacy plan that had no cleanup evidence at all.
		return ErrInvalidTarget
	}
	if a.ConfirmPodDefaultCleanup {
		if !canonical {
			return ErrInvalidTarget
		}
		if _, err := parsePodDefaultIdentities(list, a.Namespace); err != nil {
			return ErrInvalidTarget
		}
		return nil
	}
	if !canonical {
		// True legacy pvc.delete evidence has no cleanup-shaped key at all.
		return nil
	}
	identities, err := parsePodDefaultIdentities(list, a.Namespace)
	if err != nil {
		return ErrInvalidTarget
	}
	if len(identities) > 0 {
		// A non-empty reviewed cleanup set always requires the explicit
		// acknowledgement, even if a caller tries to submit the plan without it.
		return ErrInvalidTarget
	}
	return nil
}

// scanPodDefaultEvidence finds the one exact canonical top-level "podDefaults"
// field in safe prepared evidence. It rejects duplicate canonical keys,
// malformed documents and every known cleanup-shaped noncanonical alias,
// wrapper or case variant. Unknown top-level fields written by the server stay
// allowed. Aliases, wrappers and case variants are rejected as ambiguous.
func scanPodDefaultEvidence(data json.RawMessage) (value json.RawMessage, canonical, noncanonical bool, err error) {
	if len(data) == 0 {
		return nil, false, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, false, false, errors.New("prepared evidence is not valid JSON")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, false, false, errors.New("prepared evidence is not a JSON object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, false, errors.New("prepared evidence is not valid JSON")
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, false, errors.New("prepared evidence has a non-string key")
		}
		var field json.RawMessage
		if err := decoder.Decode(&field); err != nil {
			return nil, false, false, errors.New("prepared evidence is not valid JSON")
		}
		if key == "podDefaults" {
			if canonical {
				return nil, false, false, errors.New("prepared evidence repeats the canonical podDefaults key")
			}
			canonical = true
			value = field
			continue
		}
		if cleanupShapedKey(key) {
			noncanonical = true
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, false, errors.New("prepared evidence is not valid JSON")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false, false, errors.New("prepared evidence has trailing content")
	}
	return value, canonical, noncanonical, nil
}

// cleanupShapedKey recognizes noncanonical cleanup keys that must be rejected
// rather than silently ignored.
func cleanupShapedKey(key string) bool {
	switch strings.ToLower(key) {
	case "poddefaults", "poddefaultdependencies", "poddefaultnames", "poddefaultcleanuptargets", "poddefaultcleanup":
		return true
	}
	return false
}

// podDefaultIdentity is the bounded safe identity of one reviewed dependency.
// It carries no manifest, spec, injected setting, path or Secret reference.
type podDefaultIdentity struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

// podDefaultIdentitySnapshot is the immutable, bounded acceptance record of the
// exact reviewed dependency set. It is ordered by name and capped by
// MaxPodDefaultCleanup.
type podDefaultIdentitySnapshot struct {
	Version     int                  `json:"version"`
	PodDefaults []podDefaultIdentity `json:"podDefaults"`
}

// reviewedPodDefaultIdentityChange derives the single safe acceptance change
// for a confirmed cleanup. It never depends on backend change labels or wording.
func reviewedPodDefaultIdentityChange(p model.Prepared) (model.Change, bool, error) {
	a := p.Action
	if a.Kind != pvcDeleteKind || !a.ConfirmPodDefaultCleanup {
		return model.Change{}, false, nil
	}
	list, canonical, noncanonical, err := scanPodDefaultEvidence(p.Data)
	if err != nil || noncanonical || !canonical {
		return model.Change{}, false, errors.New("reviewed PodDefault identities are not available")
	}
	identities, err := parsePodDefaultIdentities(list, a.Namespace)
	if err != nil {
		return model.Change{}, false, err
	}
	value, err := json.Marshal(podDefaultIdentitySnapshot{Version: 1, PodDefaults: identities})
	if err != nil {
		return model.Change{}, false, errors.New("reviewed PodDefault identities could not be encoded")
	}
	return model.Change{Field: reviewedPodDefaultIdentitiesField, Before: "(reviewed)", After: string(value)}, true, nil
}

// reviewedPodDefaultNames reads only the names for lock derivation.
func reviewedPodDefaultNames(data json.RawMessage, namespace string) ([]string, error) {
	identities, err := reviewedPodDefaultIdentities(data, namespace)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(identities))
	for i := range identities {
		names[i] = identities[i].Name
	}
	return names, nil
}

// reviewedPodDefaultIdentities reads the exact reviewed identities from the
// canonical safe prepared evidence.
func reviewedPodDefaultIdentities(data json.RawMessage, namespace string) ([]podDefaultIdentity, error) {
	list, canonical, noncanonical, err := scanPodDefaultEvidence(data)
	if err != nil {
		return nil, err
	}
	if noncanonical {
		return nil, errors.New("reviewed PodDefault evidence uses a noncanonical cleanup key")
	}
	if !canonical {
		return nil, errors.New("reviewed PodDefault cleanup evidence is missing")
	}
	return parsePodDefaultIdentities(list, namespace)
}

// parsePodDefaultIdentities validates the sole accepted list shape:
//
//	[{"name":"mount-toast","namespace":"alice","uid":"...","resourceVersion":"..."}]
//
// name, uid and resourceVersion are required nonempty strings. namespace is
// optional and must equal the reviewed PVC namespace when present. Extra
// server-owned safe fields on an entry are ignored. The list must be explicitly
// present, even when empty.
func parsePodDefaultIdentities(list json.RawMessage, namespace string) ([]podDefaultIdentity, error) {
	if !validDNS1123Label(namespace) {
		return nil, ErrInvalidTarget
	}
	trimmed := bytes.TrimSpace(list)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("reviewed PodDefault cleanup evidence is not a list")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, fmt.Errorf("reviewed PodDefault cleanup evidence is not a list: %w", ErrInvalidTarget)
	}
	if entries == nil {
		return nil, errors.New("reviewed PodDefault cleanup evidence is not a list")
	}
	if len(entries) > MaxPodDefaultCleanup {
		return nil, fmt.Errorf("reviewed PodDefault cleanup set exceeds the bound of %d", MaxPodDefaultCleanup)
	}
	identities := make([]podDefaultIdentity, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		identity, err := decodePodDefaultDependency(entry)
		if err != nil {
			return nil, err
		}
		if identity.Namespace == "" {
			identity.Namespace = namespace
		}
		if identity.Namespace != namespace {
			return nil, errors.New("reviewed PodDefault dependency names a foreign namespace")
		}
		if _, exists := seen[identity.Name]; exists {
			return nil, errors.New("reviewed PodDefault cleanup evidence repeats a dependency")
		}
		seen[identity.Name] = struct{}{}
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Name < identities[j].Name })
	return identities, nil
}

// decodePodDefaultDependency accepts exactly one JSON object. Duplicate
// canonical entry keys and non-string canonical values fail closed; unknown
// server-owned safe fields are tolerated.
func decodePodDefaultDependency(raw json.RawMessage) (podDefaultIdentity, error) {
	var identity podDefaultIdentity
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return identity, errors.New("reviewed PodDefault dependency is malformed")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return identity, errors.New("reviewed PodDefault dependency is not an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return identity, errors.New("reviewed PodDefault dependency is malformed")
		}
		key, ok := keyToken.(string)
		if !ok {
			return identity, errors.New("reviewed PodDefault dependency is malformed")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return identity, errors.New("reviewed PodDefault dependency is malformed")
		}
		switch key {
		case "name", "namespace", "uid", "resourceVersion":
			if seen[key] {
				return identity, errors.New("reviewed PodDefault dependency repeats a canonical field")
			}
			seen[key] = true
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				return identity, errors.New("reviewed PodDefault dependency has a non-string canonical field")
			}
			switch key {
			case "name":
				identity.Name = text
			case "namespace":
				identity.Namespace = text
			case "uid":
				identity.UID = text
			case "resourceVersion":
				identity.ResourceVersion = text
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return identity, errors.New("reviewed PodDefault dependency is malformed")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return identity, errors.New("reviewed PodDefault dependency has trailing content")
	}
	if !validTargetName(identity.Name) {
		return identity, errors.New("reviewed PodDefault dependency has an invalid name")
	}
	if seen["namespace"] && identity.Namespace == "" {
		return identity, errors.New("reviewed PodDefault dependency has an invalid namespace")
	}
	if identity.Namespace != "" && !validDNS1123Label(identity.Namespace) {
		return identity, errors.New("reviewed PodDefault dependency has an invalid namespace")
	}
	if identity.UID == "" || identity.ResourceVersion == "" {
		return identity, errors.New("reviewed PodDefault dependency requires a nonempty uid and resourceVersion")
	}
	return identity, nil
}

// plannedDependencyTargets returns the secondary locks derived from the
// server-owned plan: the selected PV of a binding and the exact reviewed
// PodDefault objects. The primary target and the namespace fence are handled by
// Targets. An error means the evidence is not safe to execute.
func plannedDependencyTargets(p model.Prepared) ([]string, error) {
	a := p.Action
	var out []string
	add := func(target string) {
		for _, old := range out {
			if old == target {
				return
			}
		}
		out = append(out, target)
	}
	switch a.Kind {
	case "storage.create.static", "pv.reserve-and-claim":
		// Preserve the legacy lock shape exactly, including the old records of
		// this action that no new validator applied to.
		if a.PVName != "" {
			add("pv:" + a.PVName)
		}
	case "pvc.create.bound":
		if !validTargetName(a.PVName) {
			return nil, errors.New("a bound PVC creation has no valid selected PV")
		}
		add("pv:" + a.PVName)
	}
	if supportsPodDefaultCreation(a.Kind) && a.PodDefault != nil {
		target, err := podDefaultCreationTarget(a)
		if err != nil {
			return nil, err
		}
		add(target)
	}
	if a.Kind == pvcDeleteKind {
		// A claim bound to a Retain volume carries that volume's reclaim after
		// deletion, so the volume is fenced like any other PV write.
		volume, err := reviewedReclaimVolume(p.Data)
		if err != nil {
			return nil, err
		}
		if (volume != "") != a.ConfirmPVReclaim {
			return nil, ErrInvalidTarget
		}
		if volume != "" {
			add("pv:" + volume)
		}
	}
	if a.Kind == pvcDeleteKind && a.ConfirmPodDefaultCleanup {
		names, err := reviewedPodDefaultNames(p.Data, a.Namespace)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			target, ok := podDefaultLockTarget(a.Namespace, name)
			if !ok {
				return nil, ErrInvalidTarget
			}
			add(target)
		}
	}
	return out, nil
}

// reviewedReclaimVolume returns the Retain PersistentVolume a PVC deletion
// reclaims after the claim is gone, or "" for evidence without one. A present
// but malformed value is refused rather than silently dropping the lock.
func reviewedReclaimVolume(data json.RawMessage) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	var evidence struct {
		ReclaimVolume    *string `json:"reclaimVolume"`
		ReclaimVolumeUID *string `json:"reclaimVolumeUID"`
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		return "", errors.New("prepared evidence is not valid JSON")
	}
	if evidence.ReclaimVolume == nil && evidence.ReclaimVolumeUID == nil {
		return "", nil
	}
	if evidence.ReclaimVolume == nil || evidence.ReclaimVolumeUID == nil ||
		!validTargetName(*evidence.ReclaimVolume) || *evidence.ReclaimVolumeUID == "" {
		return "", errors.New("reviewed reclaim volume is malformed")
	}
	return *evidence.ReclaimVolume, nil
}

// validatePVCreateStatic enforces the standalone static PV contract: the PV
// name is the action name and a cluster-scoped action never carries a
// namespace, typed confirmation, observed identity or edit document.
func validatePVCreateStatic(a model.Action) error {
	if !validTargetName(a.Name) || a.Namespace != "" || a.Force || a.ConfirmName != "" ||
		a.TargetUID != "" || a.TargetResourceVersion != "" || a.StorageYAML != "" {
		return ErrInvalidTarget
	}
	if a.PVName != "" && a.PVName != a.Name {
		return ErrInvalidTarget
	}
	return nil
}

// validatePVCCreateBound enforces the bound-claim contract: an explicit
// namespace, a new claim name, one valid selected PV and the selected PV's
// reviewed UID/resourceVersion. The legacy dynamic and combined actions are not
// changed by this validator.
func validatePVCCreateBound(a model.Action) error {
	if !validTargetName(a.Name) || !validDNS1123Label(a.Namespace) || !validTargetName(a.PVName) ||
		a.Force || a.ConfirmName != "" || a.TargetUID == "" || a.TargetResourceVersion == "" ||
		a.StorageYAML != "" || a.ConfirmPodDefaultCleanup {
		return ErrInvalidTarget
	}
	return nil
}
