package auth

import "testing"

func TestNotificationScopeIsStableForSessionAndRotatesOnLogin(t *testing.T) {
	f := setup(t)
	first := findCookie(t, f.callback(f.start(t)), sessionCookie)
	response, identity := f.request("GET", "/", first, "", "", "")
	if response.Code != 204 || !validOpaque(identity.NotificationScope) {
		t.Fatal("missing opaque notification scope")
	}
	if identity.NotificationScope == first.Value || identity.NotificationScope == hash(first.Value) || identity.NotificationScope == identity.CSRF {
		t.Fatal("notification scope must not be derived from an authentication credential")
	}
	_, again := f.request("GET", "/", first, "", "", "")
	if again.NotificationScope != identity.NotificationScope {
		t.Fatal("same session must preserve dismissed warning scope")
	}
	second := findCookie(t, f.callback(f.start(t)), sessionCookie)
	_, next := f.request("GET", "/", second, "", "", "")
	if next.NotificationScope == identity.NotificationScope || next.NotificationScope == "" {
		t.Fatal("fresh login must reset dismissal scope")
	}
}
