package config

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
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
	if c.Database.Path == "" {
		return fmt.Errorf("database path required")
	}
	if c.Database.JournalMode != "" && c.Database.JournalMode != "wal" && c.Database.JournalMode != "delete" {
		return fmt.Errorf("database journalMode must be wal or delete")
	}
	if c.Kubernetes.RequestTimeoutSeconds < 1 || c.Kubernetes.RequestTimeoutSeconds > 60 {
		return fmt.Errorf("request timeout must be 1..60 seconds")
	}
	names := map[string]bool{}
	for _, t := range c.Storage.NFSTemplates {
		if t.Name == "" || names[t.Name] || t.Server == "" || !strings.HasPrefix(t.BasePath, "/") || len(t.AccessModes) == 0 {
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
