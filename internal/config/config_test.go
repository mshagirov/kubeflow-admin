package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kubeflow-admin/internal/footer"
)

const valid = `server:
  publicURL: https://admin.example.org
auth:
  issuerURL: https://login.example.org/dex
  clientID: admin
  clientSecretFile: /run/secret
  adminGroups: [platform-admins]
`

func load(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return Load(p)
}
func TestDefaultsAndStrictSchema(t *testing.T) {
	c, e := load(t, valid)
	if e != nil {
		t.Fatal(e)
	}
	if c.Features.WritesEnabled || c.Auth.SessionMinutes != 15 || c.Server.Listen != ":8080" {
		t.Fatal(c)
	}
	if _, e = load(t, valid+"unknownSecurityFlag: true\n"); e == nil {
		t.Fatal("unknown field accepted")
	}
	if _, e = load(t, valid+"---\nfeatures:\n  writesEnabled: true\n"); e == nil {
		t.Fatal("multiple documents accepted")
	}
}

func TestReclaimAndIsolationConfigurationFailsClosed(t *testing.T) {
	cfg, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Features.PVReclaim || len(cfg.Storage.IsolatedStorageClasses) != 0 {
		t.Fatal("storage exposure gates must default off")
	}
	for _, body := range []string{
		"features:\n  pvReclaim: true\n",
		"features:\n  writesEnabled: true\n  pvReclaim: true\n",
		"storage:\n  allowedStorageClasses: [approved]\n  isolatedStorageClasses: [unapproved]\n",
	} {
		if _, err := load(t, valid+body); err == nil {
			t.Fatalf("unsafe configuration accepted: %s", body)
		}
	}
}
func TestSecurityValidation(t *testing.T) {
	c, e := load(t, valid)
	if e != nil {
		t.Fatal(e)
	}
	tests := map[string]func(*Config){"http origin": func(c *Config) { c.Server.PublicURL = "http://admin.example.org" }, "path prefix": func(c *Config) { c.Server.PublicURL = "https://admin.example.org/admin" }, "empty admin": func(c *Config) { c.Auth.AdminGroups = nil }, "placeholder": func(c *Config) { c.Auth.AdminGroups = []string{"REPLACE_ME"} }, "long session": func(c *Config) { c.Auth.SessionMinutes = 60 }, "bad GPU": func(c *Config) { c.Notebooks.GPUPresets = []GPUPreset{{Name: "gpu", ResourceKey: "gpu"}} }, "bad journal": func(c *Config) { c.Database.JournalMode = "off" }, "bad login source": func(c *Config) { c.Auth.LoginSourceLimit = "trust" }, "forwarded without proxies": func(c *Config) { c.Auth.LoginSourceLimit = "forwarded" }, "malformed trusted proxy": func(c *Config) { c.Auth.TrustedProxyCIDRs = []string{"10.0.0.1"} }}
	for n, mutate := range tests {
		t.Run(n, func(t *testing.T) {
			copy := c
			mutate(&copy)
			if copy.Validate() == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}
func TestDatabaseOptions(t *testing.T) {
	for _, body := range []string{"database:\n  journalMode: delete\n  allowUnverifiedFilesystem: true\n", "database:\n  journalMode: wal\n"} {
		if _, e := load(t, valid+body); e != nil {
			t.Fatal(e)
		}
	}
}

func TestStorageFeaturesRequireWrites(t *testing.T) {
	c, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.StorageEditing || c.Features.StorageDeletion {
		t.Fatal("storage write features must be off by default")
	}
	for name, body := range map[string]string{
		"editing":  "features:\n  storageEditing: true\n",
		"deletion": "features:\n  storageDeletion: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, valid+body); err == nil {
				t.Fatal("storage feature accepted without writesEnabled")
			}
		})
	}
	if _, err := load(t, valid+"features:\n  writesEnabled: true\n  storageEditing: true\n  storageDeletion: true\n"); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryAndLoggingLimits(t *testing.T) {
	c, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.PodRestarts || c.Logging.Path != "" || c.History.WithDefaults().RetentionDays != 90 {
		t.Fatal("unsafe feature or retention defaults")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.History.RetentionDays = 91 },
		func(c *Config) { c.History.MaxAuditRecords = -1 },
		func(c *Config) { c.History.MaxAuditBytes = 1 },
		func(c *Config) { c.History.MaxOperationRecords = 1 },
		func(c *Config) { c.Logging.MaxSizeMiB = -1 },
		func(c *Config) { c.Logging.MaxBackups = 100 },
		func(c *Config) { c.Logging.MaxAgeDays = 91 },
		func(c *Config) { c.Logging.Path = "relative.log" },
		func(c *Config) { c.Logging.Path = c.Database.Path },
		func(c *Config) { c.Logging.Path = c.Database.Path + "-wal" },
	} {
		copy := c
		mutate(&copy)
		if copy.Validate() == nil {
			t.Fatal("unsafe history/logging configuration accepted")
		}
	}
}

func TestLoginSourceLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"default global", "", true},
		{"explicit global", "  loginSourceLimit: global\n", true},
		{"peer", "  loginSourceLimit: peer\n", true},
		{"forwarded with proxy", "  loginSourceLimit: forwarded\n  trustedProxyCIDRs: [10.0.0.0/8, 2001:db8::/32]\n", true},
		{"proxies without forwarded", "  trustedProxyCIDRs: [10.0.0.0/8]\n", true},
		{"unknown mode", "  loginSourceLimit: always\n", false},
		{"bare ip rejected", "  trustedProxyCIDRs: [10.0.0.1]\n", false},
		{"empty cidr rejected", "  trustedProxyCIDRs: ['']\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := load(t, valid+tc.body)
			if (e == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, e)
			}
		})
	}
}

func TestUIFooterText(t *testing.T) {
	c, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	// The footer is empty by default: a deployment never sees a shipped
	// sentence it did not ask for.
	if c.UI.FooterText != "" {
		t.Fatalf("unexpected default footer text: %q", c.UI.FooterText)
	}
	// A configured footer is kept exactly as written under the same key: the
	// console sanitizes the value before it renders it.
	c, err = load(t, valid+"ui:\n  footerText: \"Support: platform-team@example.org\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.FooterText != "Support: platform-team@example.org" {
		t.Fatalf("footer text was not loaded: %q", c.UI.FooterText)
	}
	// Branding markup is accepted under the same key and is kept verbatim in
	// the configuration: the sanitizer, not the loader, decides what is
	// rendered.
	branding := `<p><strong>Acme</strong></p><img src="https://cdn.acme.example/logo.png" alt="Acme logo"><a href="https://support.acme.example">Support</a>`
	c, err = load(t, valid+"ui:\n  footerText: "+strconv.Quote(branding)+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.FooterText != branding {
		t.Fatalf("footer branding was not loaded: %q", c.UI.FooterText)
	}
	// The section is strict like every other one: an alternative HTML footer
	// field or any other unknown key is refused instead of silently ignored.
	for _, body := range []string{
		"ui:\n  footerHtml: '<b>x</b>'\n",
		"ui:\n  footerText: 'a'\n  footerMarkup: '<b>x</b>'\n",
	} {
		if _, err = load(t, valid+body); err == nil {
			t.Fatalf("unknown ui field accepted: %q", body)
		}
	}
	// An unbounded footer is refused: the console footer is a bounded
	// branding block. The bound is counted in runes, not bytes.
	if _, err = load(t, valid+"ui:\n  footerText: "+strconv.Quote(strings.Repeat("x", footer.MaxRunes+1))+"\n"); err == nil {
		t.Fatal("oversized footer accepted")
	}
	if _, err = load(t, valid+"ui:\n  footerText: "+strconv.Quote(strings.Repeat("x", footer.MaxRunes))+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err = load(t, valid+"ui:\n  footerText: "+strconv.Quote(strings.Repeat("é", footer.MaxRunes))+"\n"); err != nil {
		t.Fatalf("a rune-counted footer at the bound was refused: %v", err)
	}
}
