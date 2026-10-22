package config

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"kubeflow-admin/internal/footer"
)

type Config struct {
	Server     Server     `yaml:"server"`
	Auth       Auth       `yaml:"auth"`
	Kubernetes Kubernetes `yaml:"kubernetes"`
	Policy     Policy     `yaml:"policy"`
	Storage    Storage    `yaml:"storage"`
	Notebooks  Notebooks  `yaml:"notebooks"`
	Database   Database   `yaml:"database"`
	Features   Features   `yaml:"features"`
	UI         UI         `yaml:"ui"`
	History    History    `yaml:"history"`
	Logging    Logging    `yaml:"logging"`
}
type Server struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"publicURL"`
}
type Auth struct {
	IssuerURL        string   `yaml:"issuerURL"`
	ClientID         string   `yaml:"clientID"`
	ClientSecretFile string   `yaml:"clientSecretFile"`
	AdminGroups      []string `yaml:"adminGroups"`
	AdminSubjects    []string `yaml:"adminSubjects"`
	Scopes           []string `yaml:"scopes"`
	SessionMinutes   int      `yaml:"sessionMinutes"`
	// LoginSourceLimit selects how the per-source login (not write) limiter keys
	// requests. Empty or "global" applies only the bounded global budget, which
	// is the fail-safe default for the shipped reverse-proxy deployment where
	// every request may share one peer address. "peer" keys by the immediate
	// peer address and is only safe when the process is directly exposed.
	// "forwarded" keys by a client address derived from X-Forwarded-For, and
	// only when the immediate peer is inside TrustedProxyCIDRs.
	LoginSourceLimit string `yaml:"loginSourceLimit"`
	// TrustedProxyCIDRs lists the reverse proxies whose X-Forwarded-For is
	// trusted in "forwarded" mode. Every entry must be valid CIDR notation.
	TrustedProxyCIDRs []string `yaml:"trustedProxyCIDRs"`
}
type Kubernetes struct {
	Kubeconfig            string `yaml:"kubeconfig"`
	RequestTimeoutSeconds int    `yaml:"requestTimeoutSeconds"`
}
type Policy struct {
	ProtectedNamespaces []string `yaml:"protectedNamespaces"`
	AllowedNamespaces   []string `yaml:"allowedNamespaces"`
}
type Storage struct {
	AllowedStorageClasses []string      `yaml:"allowedStorageClasses"`
	NFSTemplates          []NFSTemplate `yaml:"nfsTemplates"`
	CSITemplates          []CSITemplate `yaml:"csiTemplates"`
	// RegistrationTemplates is the versioned, typed template-driven static
	// provisioning contract. It is deliberately separate from the legacy
	// NFSTemplates/CSITemplates lists: deployed legacy templates keep their
	// existing PV+PVC behavior and their strict NFS descendant rule.
	RegistrationTemplates []RegistrationTemplate `yaml:"registrationTemplates"`
}

// RegistrationTemplate is one operator-approved NFS or BeeGFS registration
// template. All PV and PVC storage values are fixed server-side; the browser
// selects a template and supplies only an approved path/suffix plus the
// required NFS directory IDs where applicable. ReclaimPolicy and VolumeMode are
// accepted in the document for explicitness but only supported values are valid.
type RegistrationTemplate struct {
	Name         string `yaml:"name"`
	Kind         string `yaml:"backend"` // "nfs" or "beegfs"
	StorageClass string `yaml:"storageClass"`

	// PersistentVolume values.
	Capacity      string   `yaml:"pvCapacity"`
	AccessModes   []string `yaml:"pvAccessModes"`
	ReclaimPolicy string   `yaml:"reclaimPolicy"`
	VolumeMode    string   `yaml:"volumeMode"`

	// Independent PersistentVolumeClaim values. The request must not exceed
	// the PV capacity and every claim access mode must be supported by the PV.
	PVCRequest     string   `yaml:"pvcRequest"`
	PVCAccessModes []string `yaml:"pvcAccessModes"`

	// NFS source and approved path roots. The approved root itself and its
	// descendants are accepted for registration templates. Legacy templates
	// continue to require a strict descendant.
	NFSServer              string   `yaml:"nfsServer"`
	NFSRoots               []string `yaml:"nfsRoots"`
	MountOptions           []string `yaml:"nfsMountOptions"`
	RequireDirectoryUIDGID bool     `yaml:"requireDirectoryUIDGID"`

	// BeeGFS source. The driver and URL prefix are fixed by the operator; the
	// browser supplies only a relative suffix.
	BeeGFSDriver string `yaml:"beegfsDriver"`
	BeeGFSPrefix string `yaml:"beegfsPrefix"`

	// Optional PodDefault suggestion. The namespace and claimName are always
	// derived from the reviewed PVC, never from configuration.
	PodDefaultName            string `yaml:"podDefaultName"`
	PodDefaultMountPathPrefix string `yaml:"podDefaultMountPathPrefix"`
}

// A CSI import is a fixed, operator-approved existing volume, never a free-form handle.
type CSITemplate struct {
	Name             string            `yaml:"name"`
	Driver           string            `yaml:"driver"`
	VolumeHandle     string            `yaml:"volumeHandle"`
	StorageClass     string            `yaml:"storageClass"`
	AccessModes      []string          `yaml:"accessModes"`
	VolumeAttributes map[string]string `yaml:"volumeAttributes"`
}
type NFSTemplate struct {
	Name         string   `yaml:"name"`
	Server       string   `yaml:"server"`
	BasePath     string   `yaml:"basePath"`
	StorageClass string   `yaml:"storageClass"`
	AccessModes  []string `yaml:"accessModes"`
	MountOptions []string `yaml:"mountOptions"`
}
type Notebooks struct {
	ControllerContractConfirmed bool        `yaml:"controllerContractConfirmed"`
	GPUPresets                  []GPUPreset `yaml:"gpuPresets"`
	AllowedImages               []string    `yaml:"allowedImages"`
	ProtectedMountPaths         []string    `yaml:"protectedMountPaths"`
}
type GPUPreset struct {
	Name             string            `yaml:"name"`
	ResourceKey      string            `yaml:"resourceKey"`
	NodeSelector     map[string]string `yaml:"nodeSelector"`
	RuntimeClassName string            `yaml:"runtimeClassName"`
}
type Database struct {
	Path                      string `yaml:"path"`
	JournalMode               string `yaml:"journalMode"`
	AllowUnverifiedFilesystem bool   `yaml:"allowUnverifiedFilesystem"`
}
type Features struct {
	WritesEnabled bool `yaml:"writesEnabled"`
	StaticStorage bool `yaml:"staticStorage"`
	PVCExpansion  bool `yaml:"pvcExpansion"`
	PodRestarts   bool `yaml:"podRestarts"`
	NodeCordon    bool `yaml:"nodeCordon"`
	// ProfileDeletion is a separate opt-in for the destructive Profile
	// deletion workflow. It is never implied by WritesEnabled: both must be
	// true before the API accepts a delete request, and the console only
	// projects the capability to the browser when writes are enabled too.
	// Enabling the flag grants no Kubernetes permission by itself; the
	// reviewed RBAC grant is a separate, operator-installed step.
	ProfileDeletion bool `yaml:"profileDeletion"`
	// StorageEditing enables the restricted YAML edit workflow for a single
	// unused PersistentVolume (reclaim policy and console-owned labels) or a
	// PersistentVolumeClaim (storage increase and console-owned labels). It
	// is never implied by WritesEnabled and grants no Kubernetes permission
	// by itself.
	StorageEditing bool `yaml:"storageEditing"`
	// StorageDeletion enables the unused-only delete workflow for a single
	// PersistentVolume or PersistentVolumeClaim. It is a separate destructive
	// opt-in and is never implied by WritesEnabled or StorageEditing.
	StorageDeletion bool `yaml:"storageDeletion"`
	// PodDefaults is the separate, false-by-default opt-in for the optional
	// dedicated PodDefault substep of a PVC creation and for removing reviewed
	// PodDefault dependencies during an unused-only PVC deletion. It is never
	// implied by WritesEnabled, StaticStorage or a storage editing option.
	// Read-only PodDefault reference detection is not gated by this flag: a PVC
	// deletion must still fail closed when references exist but cleanup is
	// disabled.
	PodDefaults bool `yaml:"podDefaults"`
}

// UI holds the console presentation settings a site may set. It carries no
// capability and no permission: the footer is branding only and is reduced
// to inert markup before it is rendered.
type UI struct {
	// FooterText is the optional footer of the console. It may be plain
	// text or a small amount of HTML for branding: a company logo, a link
	// to the support portal, a short line of text. The value is sanitized
	// against a strict allowlist before it is rendered (see
	// internal/footer): script, style, embedded frames, forms, event
	// handlers, inline CSS and unsafe URL schemes are removed. The empty
	// default renders no footer at all, so a deployment never shows a
	// shipped sentence it did not ask for.
	FooterText string `yaml:"footerText"`
}

type History struct {
	RetentionDays       int   `yaml:"retentionDays"`
	MaxAuditRecords     int   `yaml:"maxAuditRecords"`
	MaxAuditBytes       int64 `yaml:"maxAuditBytes"`
	MaxOperationRecords int   `yaml:"maxOperationRecords"`
}

func (h History) WithDefaults() History {
	if h.RetentionDays == 0 {
		h.RetentionDays = 90
	}
	if h.MaxAuditRecords == 0 {
		h.MaxAuditRecords = 50000
	}
	if h.MaxAuditBytes == 0 {
		h.MaxAuditBytes = 64 << 20
	}
	if h.MaxOperationRecords == 0 {
		h.MaxOperationRecords = 100000
	}
	return h
}

type Logging struct {
	Path       string `yaml:"path"`
	MaxSizeMiB int    `yaml:"maxSizeMiB"`
	MaxBackups int    `yaml:"maxBackups"`
	MaxAgeDays int    `yaml:"maxAgeDays"`
}

func (l Logging) WithDefaults() Logging {
	if l.MaxSizeMiB == 0 {
		l.MaxSizeMiB = 10
	}
	if l.MaxBackups == 0 {
		l.MaxBackups = 5
	}
	if l.MaxAgeDays == 0 {
		l.MaxAgeDays = 7
	}
	return l
}

func Load(path string) (Config, error) {
	c := Config{Server: Server{Listen: ":8080"}, Auth: Auth{SessionMinutes: 15, Scopes: []string{"openid", "profile", "email", "groups"}}, Kubernetes: Kubernetes{RequestTimeoutSeconds: 15}, Database: Database{Path: "/var/lib/kubeflow-admin/admin.db"}}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	h := c.History.WithDefaults()
	if h.RetentionDays < 1 || h.RetentionDays > 90 || h.MaxAuditRecords < 100 || h.MaxAuditRecords > 1000000 || h.MaxAuditBytes < 1<<20 || h.MaxAuditBytes > 1<<30 || h.MaxOperationRecords < 100 || h.MaxOperationRecords > 1000000 {
		return fmt.Errorf("invalid history retention or storage limits")
	}
	l := c.Logging.WithDefaults()
	if l.MaxSizeMiB < 1 || l.MaxSizeMiB > 100 || l.MaxBackups < 1 || l.MaxBackups > 20 || l.MaxAgeDays < 1 || l.MaxAgeDays > 90 {
		return fmt.Errorf("invalid log rotation limits")
	}
	if l.Path != "" {
		if !filepath.IsAbs(l.Path) {
			return fmt.Errorf("logging path must be absolute")
		}
		db, err := filepath.Abs(c.Database.Path)
		if err != nil {
			return fmt.Errorf("invalid database path")
		}
		for _, suffix := range []string{"", ".lock", "-wal", "-shm", "-journal"} {
			if filepath.Clean(l.Path) == db+suffix {
				return fmt.Errorf("logging path must not overlap database files")
			}
		}
	}
	for _, s := range []string{c.Server.PublicURL, c.Auth.IssuerURL} {
		u, e := url.Parse(s)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("publicURL and issuerURL must be absolute HTTPS URLs")
		}
	}
	u, _ := url.Parse(c.Server.PublicURL)
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("publicURL must use a dedicated hostname, not a path prefix")
	}
	if c.Auth.ClientID == "" || c.Auth.ClientSecretFile == "" {
		return fmt.Errorf("OIDC client ID and secret file required")
	}
	if len(c.Auth.AdminGroups)+len(c.Auth.AdminSubjects) == 0 {
		return fmt.Errorf("explicit admin groups or subjects required")
	}
	for _, v := range append(append([]string{}, c.Auth.AdminGroups...), c.Auth.AdminSubjects...) {
		if strings.TrimSpace(v) == "" || strings.Contains(v, "REPLACE") {
			return fmt.Errorf("configure real admin groups/subjects")
		}
	}
	if c.Auth.SessionMinutes < 1 || c.Auth.SessionMinutes > 15 {
		return fmt.Errorf("sessionMinutes must be 1..15; re-login refreshes authorization")
	}
	switch c.Auth.LoginSourceLimit {
	case "", "global", "peer", "forwarded":
	default:
		return fmt.Errorf("loginSourceLimit must be one of global, peer or forwarded")
	}
	for _, s := range c.Auth.TrustedProxyCIDRs {
		if _, _, e := net.ParseCIDR(strings.TrimSpace(s)); e != nil {
			return fmt.Errorf("trustedProxyCIDRs must contain valid CIDR ranges")
		}
	}
	if c.Auth.LoginSourceLimit == "forwarded" && len(c.Auth.TrustedProxyCIDRs) == 0 {
		return fmt.Errorf("forwarded loginSourceLimit requires trustedProxyCIDRs")
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database path required")
	}
	if c.Database.JournalMode != "" && c.Database.JournalMode != "wal" && c.Database.JournalMode != "delete" {
		return fmt.Errorf("database journalMode must be wal or delete")
	}
	if c.Kubernetes.RequestTimeoutSeconds < 1 || c.Kubernetes.RequestTimeoutSeconds > 60 {
		return fmt.Errorf("request timeout must be 1..60 seconds")
	}
	if (c.Features.StorageEditing || c.Features.StorageDeletion || c.Features.PodDefaults) && !c.Features.WritesEnabled {
		return fmt.Errorf("storageEditing, storageDeletion and podDefaults require features.writesEnabled")
	}
	// The footer is a bounded branding block: plain text or a small amount
	// of sanitized HTML. The bound is counted in runes (Unicode code
	// points), not bytes, and it is the same bound the renderer enforces,
	// so a value that passes validation always fits the sanitized output.
	if utf8.RuneCountInString(c.UI.FooterText) > footer.MaxRunes {
		return fmt.Errorf("ui.footerText must be at most %d characters", footer.MaxRunes)
	}
	names := map[string]bool{}
	for _, t := range c.Storage.NFSTemplates {
		if t.Name == "" || names[t.Name] || t.Server == "" || !validNFSBasePath(t.BasePath) || len(t.AccessModes) == 0 {
			return fmt.Errorf("invalid or duplicate NFS storage template")
		}
		names[t.Name] = true
	}
	for _, t := range c.Storage.CSITemplates {
		if t.Name == "" || names[t.Name] || t.Driver == "" || t.VolumeHandle == "" || len(t.AccessModes) == 0 {
			return fmt.Errorf("invalid or duplicate CSI storage template")
		}
		names[t.Name] = true
	}
	for _, t := range c.Storage.RegistrationTemplates {
		if err := validateRegistrationTemplate(t, c.Storage.AllowedStorageClasses, names); err != nil {
			return err
		}
		names[t.Name] = true
	}
	names = map[string]bool{}
	for _, p := range c.Notebooks.GPUPresets {
		if p.Name == "" || names[p.Name] || !strings.Contains(p.ResourceKey, "/") {
			return fmt.Errorf("invalid or duplicate GPU preset")
		}
		names[p.Name] = true
	}
	return nil
}
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.Kubernetes.RequestTimeoutSeconds) * time.Second
}
