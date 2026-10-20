package model

import (
	"strings"
	"testing"
)

func TestDisplayNameIsBoundedPresentationNotAuthorizationKey(t *testing.T) {
	identity := Identity{Issuer: "https://identity.example.org", Subject: "opaque", Username: "  admin\n\x00\u202e ", Email: "other@example.org"}
	if identity.DisplayName() != "admin" {
		t.Fatal("display label must strip controls and whitespace")
	}
	if identity.Key() != "https://identity.example.org|opaque" {
		t.Fatal("presentation must not replace authorization identity")
	}
	identity.Username = ""
	if identity.DisplayName() != identity.Email {
		t.Fatal("missing email fallback")
	}
	identity.Username = strings.Repeat("界", 500)
	if len([]rune(identity.DisplayName())) != 160 {
		t.Fatal("display label unbounded")
	}
}
