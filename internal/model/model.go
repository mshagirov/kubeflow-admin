package model

import (
	"encoding/json"
	"time"
)

type Identity struct {
	// SessionID is the server-only hash of the opaque browser credential. It is
	// never serialized to a browser, plan, audit record, or operation.
	SessionID       string    `json:"-"`
	Issuer          string    `json:"issuer"`
	Subject         string    `json:"subject"`
	Email           string    `json:"email"`
	Groups          []string  `json:"groups"`
	AuthenticatedAt time.Time `json:"authenticatedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
	CSRF            string    `json:"csrf,omitempty"`
}

func (i Identity) Key() string { return i.Issuer + "|" + i.Subject }

// Action is deliberately typed: no arbitrary Kubernetes manifests or field paths.
type Action struct {
	Kind          string            `json:"kind"`
	Namespace     string            `json:"namespace,omitempty"`
	Name          string            `json:"name"`
	Reason        string            `json:"reason,omitempty"`
	Owner         string            `json:"owner,omitempty"`
	Quota         map[string]string `json:"quota,omitempty"`
	StorageClass  string            `json:"storageClass,omitempty"`
	Size          string            `json:"size,omitempty"`
	AccessMode    string            `json:"accessMode,omitempty"`
	PVName        string            `json:"pvName,omitempty"`
	Template      string            `json:"template,omitempty"`
	Subdirectory  string            `json:"subdirectory,omitempty"`
	Container     string            `json:"container,omitempty"`
	Image         string            `json:"image,omitempty"`
	CPURequest    string            `json:"cpuRequest,omitempty"`
	CPULimit      string            `json:"cpuLimit,omitempty"`
	MemoryRequest string            `json:"memoryRequest,omitempty"`
	MemoryLimit   string            `json:"memoryLimit,omitempty"`
	GPUPreset     string            `json:"gpuPreset,omitempty"`
	GPUCount      *int64            `json:"gpuCount,omitempty"`
	Mounts        *[]Mount          `json:"mounts,omitempty"`
	Restart       bool              `json:"restart,omitempty"`
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
	ID        string    `json:"id"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Prepared  Prepared  `json:"prepared"`
}
type Operation struct {
	ID        string    `json:"id"`
	PlanID    string    `json:"planID"`
	Actor     string    `json:"actor"`
	Target    string    `json:"target"`
	Action    string    `json:"action"`
	State     string    `json:"state"`
	Step      string    `json:"step"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type Audit struct {
	ID          string    `json:"id"`
	Actor       string    `json:"actor"`
	Action      string    `json:"action"`
	Target      string    `json:"target"`
	Result      string    `json:"result"`
	OperationID string    `json:"operationID,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Changes     []Change  `json:"changes,omitempty"`
	Time        time.Time `json:"time"`
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
