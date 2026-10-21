package kube

import (
	"strings"
	"testing"
)

// quotaPolicyHard is the hard map of a live Profile that mixes the editable
// cpu/memory aliases and one raw device key with operator-managed quota keys:
// an object count, the core quota resource itself and a storage-class capacity
// key. The latter three are preserved verbatim and are never validated against
// an adapter-side list of standard quota resources.
func quotaPolicyHard() map[string]any {
	return map[string]any{
		"cpu":            "128",
		"memory":         "256Gi",
		"nvidia.com/gpu": "4",
		"resourcequotas": int64(5),
		// "count/<resource>" is Kubernetes' reserved object-count namespace,
		// not an extended device domain.
		"count/jobs.batch": int64(7),
		"foo.storageclass.storage.k8s.io/requests.storage": "100Gi",
	}
}

// quotaPolicyRequest is the reviewed request for quotaPolicyHard: the cpu
// alias changes, every authorable key is round-tripped, and the unmanaged keys
// are supplied with semantically identical values.
func quotaPolicyRequest() map[string]string {
	return map[string]string{
		"cpu":              "96",
		"memory":           "256Gi",
		"nvidia.com/gpu":   "4",
		"resourcequotas":   "5",
		"count/jobs.batch": "7",
		"foo.storageclass.storage.k8s.io/requests.storage": "102400Mi",
	}
}

// A quota update edits the existing cpu/memory aliases and the existing raw
// device key, while every unmanaged legacy key round-trips with its exact
// original type and spelling. The adapter validates only the keys it authors;
// it never rejects a preserved operator-managed key merely because it is not
// in a duplicated upstream resource list.
func TestMergeQuotaPreservesUnmanagedLegacyKeysExactly(t *testing.T) {
	existing := quotaPolicyHard()
	merged, err := mergeQuota(existing, quotaPolicyRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != len(existing) {
		t.Fatalf("the declared key set changed: %v", merged)
	}
	if merged["cpu"] != "96" {
		t.Fatalf("the cpu alias was not edited: %v", merged)
	}
	if merged["memory"] != "256Gi" || merged["nvidia.com/gpu"] != "4" {
		t.Fatalf("an unchanged managed key was rewritten: %v", merged)
	}
	if value, ok := merged["resourcequotas"].(int64); !ok || value != 5 {
		t.Fatalf("resourcequotas lost its int64 value: %#v", merged["resourcequotas"])
	}
	if value, ok := merged["count/jobs.batch"].(int64); !ok || value != 7 {
		t.Fatalf("count/jobs.batch lost its int64 value: %#v", merged["count/jobs.batch"])
	}
	// The request supplied an equivalent 102400Mi; the declared 100Gi is
	// preserved literally rather than normalized or rewritten.
	if merged["foo.storageclass.storage.k8s.io/requests.storage"] != "100Gi" {
		t.Fatalf("the reserved storage key was not preserved literally: %#v", merged["foo.storageclass.storage.k8s.io/requests.storage"])
	}
}

// An unmanaged key may only be round-tripped. Changing the object count or the
// core quota resource is refused before any write, and neither is ever
// mistaken for an editable raw device quota.
func TestMergeQuotaRefusesChangingUnmanagedLegacyKeys(t *testing.T) {
	for _, key := range []string{"count/jobs.batch", "resourcequotas"} {
		t.Run(key, func(t *testing.T) {
			existing := quotaPolicyHard()
			if authorableQuotaKey(key, existing, nil) {
				t.Fatalf("%s was classified as authorable", key)
			}
			requested := quotaPolicyRequest()
			if key == "count/jobs.batch" {
				requested[key] = "8"
			} else {
				requested[key] = "6"
			}
			_, err := mergeQuota(existing, requested, nil)
			if err == nil || !strings.Contains(err.Error(), "changing an unsupported quota key is unsupported") {
				t.Fatalf("changing %s was accepted: %v", key, err)
			}
		})
	}
}

// A raw device quota the Profile already declares is edited under its own
// name. Whole devices are required: 4 -> 2 is a valid edit, a fraction is not,
// and an already-declared fractional value cannot be round-tripped as a
// device count.
func TestMergeQuotaEditsOnlyExistingWholeRawDeviceKeys(t *testing.T) {
	merged, err := mergeQuota(map[string]any{"nvidia.com/gpu": "4"}, map[string]string{"nvidia.com/gpu": "2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged["nvidia.com/gpu"] != "2" {
		t.Fatalf("the existing device key was not edited: %v", merged)
	}

	for name, tc := range map[string]struct {
		existing  map[string]any
		requested map[string]string
	}{
		"fractional edit": {
			existing:  map[string]any{"nvidia.com/gpu": "4"},
			requested: map[string]string{"nvidia.com/gpu": "0.5"},
		},
		"declared fraction": {
			existing:  map[string]any{"nvidia.com/gpu": "0.5"},
			requested: map[string]string{"nvidia.com/gpu": "0.5"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mergeQuota(tc.existing, tc.requested, nil)
			if err == nil || !strings.Contains(err.Error(), "a quota count must be a whole number of objects") {
				t.Fatalf("a fractional device count was accepted: %v", err)
			}
		})
	}
}

// A device key the Profile does not already declare is never introduced, even
// beside an existing editable device key.
func TestMergeQuotaRefusesNewRawDeviceKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		existing  map[string]any
		requested map[string]string
	}{
		"new key alone": {
			existing:  map[string]any{"cpu": "128"},
			requested: map[string]string{"cpu": "128", "example.com/gpu": "1"},
		},
		"new key beside existing device": {
			existing:  map[string]any{"nvidia.com/gpu": "4"},
			requested: map[string]string{"nvidia.com/gpu": "4", "example.com/gpu": "1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mergeQuota(tc.existing, tc.requested, nil)
			if err == nil || !strings.Contains(err.Error(), "unsupported quota resource") {
				t.Fatalf("a new raw device key was accepted: %v", err)
			}
		})
	}
}

// The reserved "count/<resource>" quota namespace is never a raw device
// domain. This holds even when the key is already declared, so an ordinary
// enforced object-count quota is never offered as an editable bare GPU limit
// or misclassified as an unenforced device quota.
func TestExistingDeviceQuotaKeyExcludesCountQuotaDomain(t *testing.T) {
	for _, key := range []string{
		"count/jobs.batch",
		"count/pods",
		"count/services",
		"count/persistentvolumeclaims",
	} {
		if existingDeviceQuotaKey(key) {
			t.Fatalf("%s was classified as a raw device quota", key)
		}
		existing := map[string]any{key: int64(7)}
		if authorableQuotaKey(key, existing, nil) {
			t.Fatalf("%s was offered as an authorable key", key)
		}
		if countQuotaKey(key, existing, nil) {
			t.Fatalf("%s was classified as a device count quota", key)
		}
	}
	for _, key := range []string{"nvidia.com/gpu", "example.com/gpu"} {
		if !existingDeviceQuotaKey(key) {
			t.Fatalf("%s was not classified as a raw device quota", key)
		}
	}
	for _, key := range []string{
		"requests.nvidia.com/gpu",
		"foo.storageclass.storage.k8s.io/requests.storage",
		"kubernetes.io/foo",
		"gpu",
	} {
		if existingDeviceQuotaKey(key) {
			t.Fatalf("%s was misclassified as a raw device quota", key)
		}
	}
}

// A new Profile may only introduce the builtin resources and the canonical
// requests.<device> key of an approved preset. A raw device key is refused.
func TestQuotaRefusesRawDeviceKeysForNewProfile(t *testing.T) {
	if _, err := quota(map[string]string{"cpu": "4", "memory": "8Gi"}, nil); err != nil {
		t.Fatalf("the builtin quota resources were refused: %v", err)
	}
	if _, err := quota(map[string]string{"requests.nvidia.com/gpu": "1"}, []string{"requests.nvidia.com/gpu"}); err != nil {
		t.Fatalf("the canonical preset key was refused: %v", err)
	}
	for _, key := range []string{"nvidia.com/gpu", "example.com/gpu", "count/jobs.batch", "resourcequotas"} {
		if _, err := quota(map[string]string{key: "1"}, nil); err == nil || !strings.Contains(err.Error(), "unsupported quota resource") {
			t.Fatalf("the unsupported creation key %s was accepted: %v", key, err)
		}
	}
}
