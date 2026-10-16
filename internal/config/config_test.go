package config

import (
	"os"
	"path/filepath"
	"testing"
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
