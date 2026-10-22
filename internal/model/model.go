package model

import (
	"context"
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

// MutationOutcome is the bounded outcome vocabulary of a durable mutation
// receipt. A receipt never carries a manifest, spec, path, handle, annotation or
// arbitrary setting: only the reviewed object identity and one fixed outcome.
type MutationOutcome string

const (
	MutationOutcomeCreated     MutationOutcome = "created"
	MutationOutcomeReserved    MutationOutcome = "reserved"
	MutationOutcomeOverwritten MutationOutcome = "overwritten"
	MutationOutcomeDeleted     MutationOutcome = "deleted"
	MutationOutcomeAvailable   MutationOutcome = "available"
	MutationOutcomeBound       MutationOutcome = "bound"
)

// MutationReceipt is the durable proof that one exact Kubernetes object write
// was accepted or observed. The operation recorder persists these separately
// from the mutable Step string so a crash or plan expiry cannot lose the actual
// created/reserved/overwritten/deleted identity.
type MutationReceipt struct {
	Kind            string          `json:"kind"`
	Namespace       string          `json:"namespace,omitempty"`
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Outcome         MutationOutcome `json:"outcome"`
}

type mutationReceiptRecorderKey struct{}

// WithMutationReceiptRecorder installs the operation's durable receipt sink on
// a context. The production manager installs it for real executions; direct
// backend tests may omit it, in which case RecordMutationReceipt is a no-op.
func WithMutationReceiptRecorder(ctx context.Context, recorder func(MutationReceipt) error) context.Context {
	if ctx == nil || recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, mutationReceiptRecorderKey{}, recorder)
}

// RecordMutationReceipt reports one completed write to the installed recorder.
// It is deliberately a no-op without a recorder so isolated backend tests can
// execute without a manager. A recorder error is real: callers must stop before
// the next write and must not classify the operation as rejected-before-mutation.
func RecordMutationReceipt(ctx context.Context, receipt MutationReceipt) error {
	if ctx == nil {
		return nil
	}
	recorder, ok := ctx.Value(mutationReceiptRecorderKey{}).(func(MutationReceipt) error)
	if !ok || recorder == nil {
		return nil
	}
	return recorder(receipt)
}

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
	// StorageYAML is the restricted, server-normalized projection of one
	// PersistentVolume or PersistentVolumeClaim for pv.edit and pvc.edit. It
	// never carries a full manifest or backend volume source. The projection
	// must include metadata.uid and metadata.resourceVersion, and both must
	// equal TargetUID and TargetResourceVersion, so a stale browser snapshot
	// can never overwrite a concurrent change.
	StorageYAML string `json:"storageYAML,omitempty"`
	// TargetUID and TargetResourceVersion are the exact observed identity of
	// the storage object a delete or edit action was built from. They are
	// required for the new storage actions and are never inferred from a name.
	// For pvc.create.bound they identify the selected existing PersistentVolume.
	TargetUID             string `json:"targetUID,omitempty"`
	TargetResourceVersion string `json:"targetResourceVersion,omitempty"`
	// NFSPath is the full canonical absolute NFS path entered for
	// pv.create.static. The server validates it against the selected
	// registration template's approved roots; it is never accepted from a
	// legacy template action or used as an arbitrary annotation.
	NFSPath string `json:"nfsPath,omitempty"`
	// BeeGFSPath is the canonical nonempty relative suffix entered for
	// pv.create.static. The fixed scheme, authority and prefix always come
	// from the selected registration template.
	BeeGFSPath string `json:"beeGFSPath,omitempty"`
	// DirectoryUID/DirectoryGID are the canonical nonzero decimal IDs written
	// only to the two approved NFS directory annotations on a new bound PVC.
	// They are required exactly when an NFS registration template is selected
	// and must be empty for every other template and action.
	DirectoryUID string `json:"directoryUID,omitempty"`
	DirectoryGID string `json:"directoryGID,omitempty"`
	// PodDefault is the optional, single dedicated PodDefault substep of a
	// PVC creation. There is no standalone PodDefault mutation action.
	PodDefault *PodDefaultRequest `json:"podDefault,omitempty"`
	// ConfirmPodDefaultCleanup acknowledges removal of the exact related
	// namespace-local PodDefaults reviewed by a pvc.delete plan. It is only
	// valid when that plan carries a complete bounded cleanup set.
	ConfirmPodDefaultCleanup bool `json:"confirmPodDefaultCleanup,omitempty"`
}

// PodDefaultRequest is the typed browser input for the optional dedicated
// PodDefault of a PVC creation. Namespace and claimName are never supplied here:
// both are fixed to the reviewed PVC. Overwrite is an explicit acknowledgement
// and is required only when the reviewed name already exists.
type PodDefaultRequest struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	Overwrite bool   `json:"overwrite"`
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
	// MutationReceipts is the bounded, append-only identity proof of accepted
	// cluster writes. It survives plan expiry and process restart independently
	// of Step and Error; the manager/store own validation and persistence.
	MutationReceipts []MutationReceipt `json:"mutationReceipts,omitempty"`
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

// StorageDocument is the safe, server-produced projection of one
// PersistentVolume or PersistentVolumeClaim. It never contains a volume source,
// backend path/handle/attribute, Secret reference, annotation, finalizer,
// owner reference, StorageClass or status. YAML is the strict editable form;
// UID and ResourceVersion must be echoed back in the edit action.
type StorageDocument struct {
	Kind            string   `json:"kind"`
	Namespace       string   `json:"namespace,omitempty"`
	Name            string   `json:"name"`
	UID             string   `json:"uid"`
	ResourceVersion string   `json:"resourceVersion"`
	YAML            string   `json:"yaml"`
	Warnings        []string `json:"warnings,omitempty"`
}

// StorageRegistrationTemplate is the browser-safe projection of one configured
// registration template. It carries only approved read-only storage values and
// suggestions. It never carries an existing PV backend path, handle, Secret,
// unmanaged driver attribute or credential.
type StorageRegistrationTemplate struct {
	Name                      string   `json:"name"`
	Kind                      string   `json:"kind"`
	StorageClass              string   `json:"storageClass"`
	PVCapacity                string   `json:"pvCapacity"`
	PVAccessModes             []string `json:"pvAccessModes"`
	PVCRequest                string   `json:"pvcRequest"`
	PVCAccessModes            []string `json:"pvcAccessModes"`
	ReclaimPolicy             string   `json:"reclaimPolicy"`
	VolumeMode                string   `json:"volumeMode"`
	NFSServer                 string   `json:"nfsServer,omitempty"`
	NFSRoots                  []string `json:"nfsRoots,omitempty"`
	NFSMountOptions           []string `json:"nfsMountOptions,omitempty"`
	RequireDirectoryUIDGID    bool     `json:"requireDirectoryUIDGID,omitempty"`
	BeeGFSDriver              string   `json:"beegfsDriver,omitempty"`
	BeeGFSPrefix              string   `json:"beegfsPrefix,omitempty"`
	PodDefaultName            string   `json:"podDefaultName,omitempty"`
	PodDefaultMountPathPrefix string   `json:"podDefaultMountPathPrefix,omitempty"`
}

type Inventory struct {
	ObservedAt time.Time  `json:"observedAt"`
	Resources  []Resource `json:"resources"`
	Warnings   []string   `json:"warnings,omitempty"`
	// StorageTemplates is present only when writes and static storage are both
	// enabled. It is the same approved projection used by the storage forms.
	StorageTemplates []StorageRegistrationTemplate `json:"storageTemplates,omitempty"`
}
