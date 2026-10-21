package model

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
)

// ErrRejectedBeforeMutation is used only when the backend can establish that
// the API rejected an action before any mutation occurred. Ambiguous transport
// failures must never use it.
var ErrRejectedBeforeMutation = errors.New("action rejected before mutation")

type Identity struct {
	// SessionID is the server-only hash of the opaque browser credential. It is
	// never serialized to a browser, plan, audit record, or operation.
	SessionID         string    `json:"-"`
	Issuer            string    `json:"issuer"`
	Subject           string    `json:"subject"`
	Email             string    `json:"email"`
	Username          string    `json:"username,omitempty"`
	NotificationScope string    `json:"notificationScope,omitempty"`
	Groups            []string  `json:"groups"`
	AuthenticatedAt   time.Time `json:"authenticatedAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	CSRF              string    `json:"csrf,omitempty"`
}

func (i Identity) Key() string { return i.Issuer + "|" + i.Subject }

// DisplayName is a presentation label from signed provider claims, never an
// authorization key. Historical records capture it at acceptance time.
func (i Identity) DisplayName() string {
	for _, value := range []string{i.Username, i.Email, i.Subject} {
		value = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, value))
		if value != "" {
			runes := []rune(value)
			if len(runes) > 160 {
				runes = runes[:160]
			}
			return string(runes)
		}
	}
	return "Unknown administrator"
}

// Action is deliberately typed: no arbitrary Kubernetes manifests or field paths.
type Action struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Reason    string `json:"reason,omitempty"`
	// ConfirmName is the exact target name typed back by the administrator as a
	// second confirmation. It is required for destructive kinds such as
	// profile.delete and is ignored by every other kind.
	ConfirmName string `json:"confirmName,omitempty"`
	// BaseConfigDigest is the canonical digest of the Profile configuration
	// the administrator reviewed: the owner identity and the declared quota
	// hard map. It is echoed back from the console's own observation and is
	// required by profile.update, so a stale prefill cannot overwrite a
	// concurrent quota change. It is never an editable field.
	BaseConfigDigest string            `json:"baseConfigDigest,omitempty"`
	Owner            string            `json:"owner,omitempty"`
	Quota            map[string]string `json:"quota,omitempty"`
	StorageClass     string            `json:"storageClass,omitempty"`
	Size             string            `json:"size,omitempty"`
	AccessMode       string            `json:"accessMode,omitempty"`
	PVName           string            `json:"pvName,omitempty"`
	Template         string            `json:"template,omitempty"`
	Subdirectory     string            `json:"subdirectory,omitempty"`
	Container        string            `json:"container,omitempty"`
	Image            string            `json:"image,omitempty"`
	CPURequest       string            `json:"cpuRequest,omitempty"`
	CPULimit         string            `json:"cpuLimit,omitempty"`
	MemoryRequest    string            `json:"memoryRequest,omitempty"`
	MemoryLimit      string            `json:"memoryLimit,omitempty"`
	GPUPreset        string            `json:"gpuPreset,omitempty"`
	GPUCount         *int64            `json:"gpuCount,omitempty"`
	Mounts           *[]Mount          `json:"mounts,omitempty"`
	Restart          bool              `json:"restart,omitempty"`
	Force            bool              `json:"force,omitempty"`
}
type Mount struct {
	Name     string `json:"name"`
	Claim    string `json:"claim"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly"`
}
type Prepared struct {
	Action          Action   `json:"action"`
	TargetUID       string   `json:"targetUID"`
	ResourceVersion string   `json:"resourceVersion"`
	Summary         string   `json:"summary"`
	Warnings        []string `json:"warnings"`
	Changes         []Change `json:"changes,omitempty"`
	ParentNotebook  string   `json:"parentNotebook,omitempty"`
	// Data must contain only safe, narrow edit/dependency evidence, never whole Pod templates.
	Data json.RawMessage `json:"data,omitempty"`
}

// Change contains only approved editable fields, not full workload manifests.
type Change struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type Plan struct {
	ID           string    `json:"id"`
	Actor        string    `json:"actor"`
	ActorDisplay string    `json:"actorDisplay,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Prepared     Prepared  `json:"prepared"`
}
type Operation struct {
	ID             string `json:"id"`
	PlanID         string `json:"planID"`
	Actor          string `json:"actor"`
	ActorDisplay   string `json:"actorDisplay,omitempty"`
	HistoryExpired bool   `json:"historyExpired,omitempty"`
	Target         string `json:"target"`
	Action         string `json:"action"`
	State          string `json:"state"`
	Step           string `json:"step"`
	Error          string `json:"error,omitempty"`
	// ProfileDeleteCheckpointed records that the durable checkpoint which
	// precedes the single Profile delete request was persisted before that
	// request was issued. It is the durable proof that no Profile deletion
	// write was ever started, and the only evidence that may let an offline
	// reconciliation cancel an uncertain deletion. It is always serialized,
	// so a record that lacks the field is distinguishable from a record that
	// was admitted without one.
	ProfileDeleteCheckpointed bool      `json:"profileDeleteCheckpointed"`
	CreatedAt                 time.Time `json:"createdAt"`
	UpdatedAt                 time.Time `json:"updatedAt"`
}
type Audit struct {
	ID           string    `json:"id"`
	Actor        string    `json:"actor"`
	ActorDisplay string    `json:"actorDisplay,omitempty"`
	Action       string    `json:"action"`
	Target       string    `json:"target"`
	Result       string    `json:"result"`
	OperationID  string    `json:"operationID,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Changes      []Change  `json:"changes,omitempty"`
	Time         time.Time `json:"time"`
}
type Resource struct {
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	UID       string            `json:"uid,omitempty"`
	Owner     string            `json:"owner,omitempty"`
	State     string            `json:"state,omitempty"`
	Desired   string            `json:"desired,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}
type Inventory struct {
	ObservedAt time.Time  `json:"observedAt"`
	Resources  []Resource `json:"resources"`
	Warnings   []string   `json:"warnings,omitempty"`
}
