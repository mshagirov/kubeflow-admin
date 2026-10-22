package web

import (
	"context"
	"html/template"
	"reflect"
	"strings"
	"testing"

	"kubeflow-admin/internal/footer"
)

// footerSection returns the rendered footer element of a page, or an empty
// string when the page has none. The assertions look at the footer alone:
// the console shell carries its own script and style elements.
func footerSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<footer>")
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</footer>")
	if end < 0 {
		return ""
	}
	return body[start : start+end+len("</footer>")]
}

// The console footer is deployment configuration, not a shipped sentence. The
// default is empty, so no footer element is rendered at all and the page
// keeps no empty placeholder.
func TestFooterIsEmptyByDefault(t *testing.T) {
	s, _, _ := fixture()
	body := request(handler(s), "GET", "/", "").Body.String()
	if strings.Contains(body, "<footer") {
		t.Fatal("an empty footer must not render a footer element")
	}
	for _, gone := range []string{
		"Administrative changes are audited",
		"Inventory reflects its observation timestamp",
		"History is a bounded recent window, not a full export",
	} {
		if strings.Contains(body, gone) {
			t.Fatalf("the shipped footer text is still rendered: %q", gone)
		}
	}
	// A whitespace-only value is no footer either.
	s.cfg.UI.FooterText = "   \n\t  "
	body = request(handler(s), "GET", "/", "").Body.String()
	if strings.Contains(body, "<footer") {
		t.Fatal("a whitespace-only footer must not render a footer element")
	}
	// A value that sanitizes to nothing is no footer either.
	s.cfg.UI.FooterText = `<script>alert(1)</script>`
	body = request(handler(s), "GET", "/", "").Body.String()
	if strings.Contains(body, "<footer") {
		t.Fatal("a footer that sanitizes to nothing must not render a footer element")
	}
}

// A configured footer is rendered as sanitized branding: the company logo,
// the support link and the text are shown, and the link is a hardened
// external navigation.
func TestFooterRendersSanitizedBrandingHTML(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `<div><p><strong>Acme Platform</strong> · Support</p>` +
		`<a href="https://support.acme.example">Support portal</a>` +
		`<img src="https://cdn.acme.example/logo.png" alt="Acme logo" width="120" height="40"></div>`
	body := request(handler(s), "GET", "/", "").Body.String()
	if !strings.Contains(body, "<footer>") || !strings.Contains(body, "</footer>") {
		t.Fatalf("the configured footer is not rendered in a footer element: %s", body)
	}
	for _, kept := range []string{
		"<strong>Acme Platform</strong>",
		`href="https://support.acme.example"`,
		`src="https://cdn.acme.example/logo.png"`,
		`alt="Acme logo"`,
		`target="_blank"`,
		`rel="noopener noreferrer"`,
	} {
		if !strings.Contains(body, kept) {
			t.Fatalf("the configured branding lost %q: %s", kept, body)
		}
	}
	if strings.Count(body, "<footer>") != 1 {
		t.Fatal("the footer must be rendered exactly once")
	}
}

// A plain configured footer keeps working: the value is shown as text.
func TestFooterPlainTextStaysText(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `Support: platform-team@example.org`
	body := request(handler(s), "GET", "/", "").Body.String()
	if !strings.Contains(body, "Support: platform-team@example.org") {
		t.Fatalf("the configured footer text is missing from the page: %s", body)
	}
	// A legacy plain-text value is escaped, not interpreted.
	s.cfg.UI.FooterText = `Support A & B <not a tag>`
	footer := footerSection(t, request(handler(s), "GET", "/", "").Body.String())
	if !strings.Contains(footer, "Support A &amp; B") {
		t.Fatalf("the configured footer text was not escaped: %s", footer)
	}
	if strings.Contains(footer, "<not a tag>") {
		t.Fatalf("the configured footer text became markup: %s", footer)
	}
}

// Basic formatting in the configured footer is now rendered as formatting:
// the key accepts a small amount of HTML for branding.
func TestFooterFormattingIsRendered(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `<p><strong>Acme</strong> <em>Platform</em></p>`
	footer := footerSection(t, request(handler(s), "GET", "/", "").Body.String())
	if !strings.Contains(footer, "<strong>Acme</strong>") || !strings.Contains(footer, "<em>Platform</em>") {
		t.Fatalf("the configured formatting was not rendered: %s", footer)
	}
}

// Unsafe markup in the configured footer never reaches the browser.
func TestFooterRemovesUnsafeMarkup(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `<p>Support</p>` +
		`<script>alert(1)</script>` +
		`<img src="x" onerror="alert(1)">` +
		`<a href="javascript:alert(1)">click</a>` +
		`<img src="data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=">` +
		`<iframe src="https://evil.example"></iframe>` +
		`<div onclick="alert(1)" style="color:red">text</div>`
	body := request(handler(s), "GET", "/", "").Body.String()
	footer := footerSection(t, body)
	if !strings.Contains(footer, "Support") {
		t.Fatal("the safe part of the footer is missing from the page")
	}
	for _, gone := range []string{"<script", "alert(1)", "onerror", "onclick", "javascript:", "data:image", "<iframe", "style=", "evil.example"} {
		if strings.Contains(footer, gone) {
			t.Fatalf("unsafe footer content reached the page: %q", gone)
		}
	}
}

// Malformed markup is reduced to something safe instead of failing the page.
func TestFooterMalformedMarkupIsSafe(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `<p>unclosed <b>bold<img src="https://cdn.acme.example/logo.png" alt="x"`
	footer := footerSection(t, request(handler(s), "GET", "/", "").Body.String())
	if footer == "" {
		t.Fatal("the malformed footer was not rendered")
	}
	if strings.Contains(strings.ToLower(footer), "<script") || strings.Contains(strings.ToLower(footer), "onerror") {
		t.Fatalf("malformed footer markup produced something unsafe: %s", footer)
	}
}

// The projection stays minimal and carries only sanitized markup: the raw
// configured value is never projected, and the one trusted field is the
// sanitized HTML.
func TestFooterProjectionCarriesOnlySanitizedHTML(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `Site footer <script>alert(1)</script>`
	s.cfg.Auth.ClientID = "site-client-id"
	s.cfg.Auth.ClientSecretFile = "/var/run/app-secrets/oidc-client-secret"
	s.cfg.Auth.IssuerURL = "https://identity.example.org/dex"
	s.cfg.Auth.AdminGroups = []string{"platform-admins"}
	s.cfg.Database.Path = "/var/lib/kubeflow-admin/admin.db"
	s.cfg.Kubernetes.Kubeconfig = "/etc/kubeflow-admin/kubeconfig"

	identity, ok := s.identity(context.Background())
	if !ok {
		t.Fatal("fixture identity missing")
	}
	view := buildPageView(identity, s.cfg)
	if view.Config.UI.FooterText != "Site footer" {
		t.Fatalf("the projected footer is not the sanitized markup: %q", view.Config.UI.FooterText)
	}
	if strings.Contains(string(view.Config.UI.FooterText), "script") {
		t.Fatalf("the projection carries raw configured markup: %q", view.Config.UI.FooterText)
	}
	// The projected UI section is the sanitized footer and nothing else, and
	// it is the typed safe HTML that the template trusts.
	uiType := reflect.TypeOf(pageUI{})
	if uiType.NumField() != 1 || uiType.Field(0).Name != "FooterText" || uiType.Field(0).Type != reflect.TypeOf(template.HTML("")) {
		t.Fatalf("the UI projection must carry the sanitized footer only: %+v", uiType)
	}
	// The projected configuration keeps exactly its known sections.
	configType := reflect.TypeOf(pageConfig{})
	known := map[string]bool{"Features": true, "Notebooks": true, "Storage": true, "UI": true}
	if configType.NumField() != len(known) {
		t.Fatalf("unexpected projected configuration: %+v", configType)
	}
	for i := 0; i < configType.NumField(); i++ {
		if !known[configType.Field(i).Name] {
			t.Fatalf("unexpected projected configuration field: %s", configType.Field(i).Name)
		}
	}
	// The page view carries the page, its configuration and the response
	// policy, and nothing else.
	viewType := reflect.TypeOf(pageView{})
	viewFields := map[string]bool{"Identity": true, "Config": true, "ContentSecurityPolicy": true}
	if viewType.NumField() != len(viewFields) {
		t.Fatalf("unexpected page view: %+v", viewType)
	}
	for i := 0; i < viewType.NumField(); i++ {
		if !viewFields[viewType.Field(i).Name] {
			t.Fatalf("unexpected page view field: %s", viewType.Field(i).Name)
		}
	}
	// The rendered page carries the footer and none of the configuration.
	body := request(handler(s), "GET", "/", "").Body.String()
	if !strings.Contains(body, "Site footer") {
		t.Fatal("the configured footer is missing from the page")
	}
	if strings.Contains(body, "alert(1)") {
		t.Fatal("the raw configured footer reached the page")
	}
	for _, secret := range []string{
		"site-client-id",
		"/var/run/app-secrets/oidc-client-secret",
		"https://identity.example.org/dex",
		"platform-admins",
		"/var/lib/kubeflow-admin/admin.db",
		"/etc/kubeflow-admin/kubeconfig",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("the page carries configuration it must never project: %q", secret)
		}
	}
}

// imageSources returns the sources of the img-src directive of a page
// policy.
func imageSources(policy string) []string {
	for _, directive := range strings.Split(policy, ";") {
		fields := strings.Fields(directive)
		if len(fields) == 0 || fields[0] != "img-src" {
			continue
		}
		return fields[1:]
	}
	return nil
}

// The page policy names exactly the external image hosts the configured
// footer references. A deployment without a footer keeps the shipped policy.
func TestFooterPagePolicyNamesOnlyConfiguredImageHosts(t *testing.T) {
	s, _, _ := fixture()
	shipped := request(handler(s), "GET", "/", "").Header().Get("Content-Security-Policy")
	if !strings.Contains(shipped, "img-src 'self'") {
		t.Fatalf("the shipped page policy changed: %s", shipped)
	}
	if strings.Contains(shipped, "acme.example") {
		t.Fatalf("the shipped page policy names an unconfigured host: %s", shipped)
	}
	s.cfg.UI.FooterText = `<img src="https://cdn.acme.example/logo.png" alt="Acme logo">`
	policy := request(handler(s), "GET", "/", "").Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src 'self' https://cdn.acme.example") {
		t.Fatalf("the page policy does not name the configured image host: %s", policy)
	}
	if !strings.Contains(policy, "script-src 'self'") || !strings.Contains(policy, "default-src 'none'") {
		t.Fatalf("the page policy lost its shipped rules: %s", policy)
	}
	// A link never opens the image source.
	s.cfg.UI.FooterText = `<a href="https://elsewhere.acme.example/">Elsewhere</a>`
	policy = request(handler(s), "GET", "/", "").Header().Get("Content-Security-Policy")
	if strings.Contains(policy, "elsewhere.acme.example") {
		t.Fatalf("a link opened an image source: %s", policy)
	}
}

// The page policy keeps its shipped rules and names only plain image
// sources: no wildcard, no directive separator, no quoted token and no
// whitespace may reach the response header.
func TestFooterPagePolicyNamesPlainImageSources(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `<img src="https://cdn.acme.example/logo.png" alt="Acme logo">` +
		`<img src="http://legacy.acme.example/logo.png" alt="Legacy">` +
		`<img src="//static.acme.example/logo.png" alt="Static">` +
		`<img src="/static/logo.png" alt="Local">`
	response := request(handler(s), "GET", "/", "")
	policy := response.Header().Get("Content-Security-Policy")
	for _, host := range []string{"https://cdn.acme.example", "http://legacy.acme.example", "https://static.acme.example"} {
		if !strings.Contains(policy, " "+host) {
			t.Fatalf("the page policy does not name the configured image host %q: %s", host, policy)
		}
	}
	for _, kept := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(policy, kept) {
			t.Fatalf("the page policy lost %q: %s", kept, policy)
		}
	}
	for _, source := range imageSources(policy) {
		if strings.HasPrefix(source, "'") {
			// A quoted keyword such as 'self' is not an image host.
			continue
		}
		for _, bad := range []string{";", "'", "\"", " ", "\t", "\n", "\r", "*"} {
			if strings.Contains(source, bad) {
				t.Fatalf("the page policy names an unsafe image source %q: %s", source, policy)
			}
		}
	}
}

// An image source that cannot be named exactly in the page policy opens
// nothing: the page keeps its shipped policy and renders no image.
func TestFooterPagePolicyRejectsUnsafeImageHosts(t *testing.T) {
	s, _, _ := fixture()
	for _, src := range []string{
		"https://cdn.acme.example;script-src*",
		"https://cdn.acme.example;script-src 'unsafe-inline' https://evil.example",
		"https://*.acme.example/logo.png",
		"https://user:pass@cdn.acme.example/logo.png",
		"https://cdn.acme.example:99999/logo.png",
		"https://cdn.acme.example:0/logo.png",
		"https://cdn.acme.example:/logo.png",
	} {
		s.cfg.UI.FooterText = `<img src="` + src + `" alt="Acme logo">`
		response := request(handler(s), "GET", "/", "")
		policy := response.Header().Get("Content-Security-Policy")
		if policy != contentSecurityPolicy(nil) {
			t.Fatalf("an unsafe image host changed the page policy: %s", policy)
		}
		if strings.Contains(policy, "acme.example") || strings.Contains(policy, "evil.example") {
			t.Fatalf("an unsafe image host reached the page policy: %s", policy)
		}
		if strings.Contains(response.Body.String(), "<footer") {
			t.Fatalf("an unsafe image rendered a footer: %s", response.Body.String())
		}
	}
}

// A scheme-relative image is normalized to https, so the reference the
// browser requests and the origin the policy opens are the same one.
func TestFooterSchemeRelativeImageIsNormalized(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.UI.FooterText = `<img src="//cdn.acme.example/logo.png" alt="Acme logo">`
	response := request(handler(s), "GET", "/", "")
	if !strings.Contains(response.Body.String(), `src="https://cdn.acme.example/logo.png"`) {
		t.Fatalf("a scheme-relative image was not normalized: %s", response.Body.String())
	}
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src 'self' https://cdn.acme.example") {
		t.Fatalf("the page policy does not name the normalized image host: %s", policy)
	}
}

// The Overview page no longer carries the static explanation panels, and the
// observed Workspace Health content remains without a separate metrics row.
func TestOverviewExplanationPanelsAreRemoved(t *testing.T) {
	s, _, _ := fixture()
	body := request(handler(s), "GET", "/", "").Body.String()
	for _, gone := range []string{"Review before you apply", "Observed, not assumed", "info-grid", `id="metrics"`} {
		if strings.Contains(body, gone) {
			t.Fatalf("the removed Overview panel is still rendered: %q", gone)
		}
	}
	// The Overview page itself keeps its observed content.
	for _, kept := range []string{`id="overview" class="page"`, `id="workspace-health-summary"`, `id="overview-table" data-table="overview"`} {
		if !strings.Contains(body, kept) {
			t.Fatalf("the Overview page lost %q", kept)
		}
	}
}

// The plan confirmation workflow is unchanged by the footer work: the
// browser still confirms a server-owned plan before anything is submitted.
func TestPlanConfirmationWorkflowIsUnchanged(t *testing.T) {
	s, _, o := fixture()
	body := request(handler(s), "GET", "/", "").Body.String()
	for _, control := range []string{`id="plan-dialog"`, `id="plan-preview"`, `id="confirm-plan"`, `id="cancel-plan"`, `id="submit-plan"`} {
		if !strings.Contains(body, control) {
			t.Fatalf("the plan confirmation control %s is missing", control)
		}
	}
	response := request(handler(s), "POST", "/api/v1/plans", `{"kind":"profile.update","name":"team","owner":"owner@example.org","baseConfigDigest":"sha256:reviewed","quota":{"requests.cpu":"4","pods":"0"}}`)
	if response.Code != 201 || o.plans != 1 {
		t.Fatalf("the plan workflow changed: %d %s", response.Code, response.Body.String())
	}
}

// The renderer bound and the configuration bound are the same bound.
func TestFooterBoundMatchesTheConfigurationBound(t *testing.T) {
	if footer.MaxRunes != 16384 {
		t.Fatalf("the footer bound changed: %d", footer.MaxRunes)
	}
}
