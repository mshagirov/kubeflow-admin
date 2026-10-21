package web

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfileValidationMessagesAreActionable(t *testing.T) {
	for _, message := range []string{
		"Profile name must be at most 63 characters",
		"Profile name must be a lowercase DNS label",
		"a valid owner email address is required",
		"Profile owner transfer is unsupported by the installed controller contract",
		"the reviewed Profile configuration digest is required",
		"Profile quota already matches the requested values",
		"a quota value must be a non-negative Kubernetes quantity",
		"a quota count must be a whole number of objects",
		"Reload the console and review the current Profile configuration before changing quotas",
		"profile deletion is disabled",
		"a nonempty reason is required for profile deletion",
		"typed confirmation must exactly match the Profile name",
		"profile deletion accepts no other action fields",
		"the Profile namespace does not exist; this deletion requires the Profile-owned namespace",
		"the Profile namespace has no stable UID",
		"Profile has no stable UID",
		"unsupported Profile API contract",
		"cluster identity could not be verified",
		"the namespace list could not be verified",
		"the Profile namespace has more than one owner reference",
		"Profile deletion requires UID and resourceVersion preconditions",
	} {
		t.Run(message, func(t *testing.T) {
			response := httptest.NewRecorder()
			backendFailure(response, errors.New(message), true)
			if response.Code != 422 || !strings.Contains(response.Body.String(), message) {
				t.Fatalf("validation message was not actionable: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestProfileUpdateCarriesExactOwnerAndReviewedConfiguration(t *testing.T) {
	s, _, operations := fixture()
	body := `{"kind":"profile.update","name":"team","owner":"Legacy-Owner","baseConfigDigest":"reviewed-config","quota":{"requests.cpu":"4","pods":"0"}}`
	response := request(handler(s), "POST", "/api/v1/plans", body)
	if response.Code != 201 {
		t.Fatalf("typed update was not decoded: %d %s", response.Code, response.Body.String())
	}
	if operations.action.Kind != "profile.update" || operations.action.Owner != "Legacy-Owner" || operations.action.BaseConfigDigest != "reviewed-config" || operations.action.Quota["pods"] != "0" {
		t.Fatalf("typed update changed its preconditions: %+v", operations.action)
	}
}

func TestProfileDeletionDryRunRefusalsAreActionable(t *testing.T) {
	for _, tc := range []struct {
		message string
		status  int
		code    string
	}{
		{"Kubernetes API denied the Profile deletion", 503, "cluster_access_denied"},
		{"Kubernetes API rejected the Profile deletion", 422, "cluster_validation_failed"},
	} {
		response := httptest.NewRecorder()
		backendFailure(response, errors.New(tc.message), true)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.code) || !strings.Contains(response.Body.String(), `"retryable":false`) {
			t.Fatalf("dry-run refusal was not actionable: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestProfileDeletePreservesRawConfirmation(t *testing.T) {
	s, _, operations := fixture()
	s.cfg.Features.ProfileDeletion = true
	response := request(handler(s), "POST", "/api/v1/plans", `{"kind":"profile.delete","name":"team","reason":"pilot cleanup","confirmName":" team "}`)
	if response.Code != 201 || operations.action.ConfirmName != " team " {
		t.Fatalf("HTTP decoding must not normalize the confirmation: %d %+v", response.Code, operations.action)
	}
	// This fixture only tests decoding. The real Kubernetes adapter must reject
	// this padded value before any mutation, rather than receiving "team".
}
