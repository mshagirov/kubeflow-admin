package config

import "testing"

func TestNFSBasePathRequiresCanonicalNonRootDirectory(t *testing.T) {
	base, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		ok   bool
	}{
		{"/exports/approved-user", true},
		{"/prepared", true},
		{"/", false},
		{"", false},
		{"relative", false},
		{"/prepared/../other", false},
		{"/prepared//user", false},
		{"/prepared/user/", false},
		{"/.", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			cfg := base
			cfg.Storage.NFSTemplates = []NFSTemplate{{Name: "approved", Server: "nfs.example.org", BasePath: test.path, AccessModes: []string{"ReadWriteMany"}}}
			err := cfg.Validate()
			if (err == nil) != test.ok {
				t.Fatalf("basePath=%q: error=%v, want valid=%v", test.path, err, test.ok)
			}
		})
	}
}
