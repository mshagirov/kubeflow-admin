package footer

import (
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/html"
)

// imageHTML builds an image element whose source is the given value. The
// value is escaped, so a test may use characters that are not valid in an
// attribute value.
func imageHTML(src string) string {
	return `<img src="` + html.EscapeString(src) + `" alt="Acme logo">`
}

// The allowlist keeps the branding markup a deployment needs: basic
// formatting, lists, links and images.
func TestSanitizeKeepsBrandingMarkup(t *testing.T) {
	in := `<div class="x"><p><strong>Acme</strong> <em>Platform</em> <b>Team</b> <i>Support</i></p>` +
		`<ul><li>One</li><li>Two</li></ul><ol><li>First</li></ol><span>Note</span><br>` +
		`<a href="https://support.acme.example">Support portal</a>` +
		`<img src="https://cdn.acme.example/logo.png" alt="Acme logo" title="Acme" width="120" height="40"></div>`
	out := string(Sanitize(in).HTML)
	for _, keep := range []string{
		"<strong>Acme</strong>", "<em>Platform</em>", "<b>Team</b>", "<i>Support</i>",
		"<ul>", "<li>One</li>", "<ol>", "<li>First</li>", "<span>Note</span>", "<br",
		`href="https://support.acme.example"`, `src="https://cdn.acme.example/logo.png"`,
		`alt="Acme logo"`, `title="Acme"`, `width="120"`, `height="40"`,
	} {
		if !strings.Contains(out, keep) {
			t.Fatalf("the allowlist dropped %q from %s", keep, out)
		}
	}
	// The class attribute is not on the allowlist: the footer cannot restyle
	// the console.
	if strings.Contains(out, "class=") {
		t.Fatalf("an unlisted attribute survived: %s", out)
	}
}

// Everything outside the allowlist is removed from the value: script and
// style elements with their content, embedded frames, objects, forms, event
// handlers, inline CSS and raw SVG.
func TestSanitizeRemovesUnsafeMarkup(t *testing.T) {
	for _, in := range []string{
		`<script>alert(1)</script>`,
		`<style>body{display:none}</style>`,
		`<iframe src="https://evil.example"></iframe>`,
		`<object data="https://evil.example"></object>`,
		`<embed src="https://evil.example">`,
		`<form action="https://evil.example"><input name="a"></form>`,
		`<div onclick="alert(1)">text</div>`,
		`<div onmouseover="alert(1)">text</div>`,
		`<img src="https://cdn.acme.example/l.png" onerror="alert(1)">`,
		`<p style="background:url(javascript:alert(1))">text</p>`,
		`<svg><script>alert(1)</script></svg>`,
		`<meta http-equiv="refresh" content="0;url=https://evil.example">`,
		`<base href="https://evil.example/">`,
	} {
		out := string(Sanitize(in).HTML)
		for _, gone := range []string{"<script", "<style", "<iframe", "<object", "<embed", "<form", "<input", "onclick", "onmouseover", "onerror", "style=", "<svg", "<meta", "<base", "javascript:", "evil.example"} {
			if strings.Contains(strings.ToLower(out), gone) {
				t.Fatalf("unsafe markup %q survived in %q", gone, out)
			}
		}
	}
}

// A link never carries an executable destination and always opens as a
// hardened external navigation.
func TestSanitizeLinksAreHardened(t *testing.T) {
	for _, in := range []string{
		`<a href="javascript:alert(1)">x</a>`,
		`<a href="JaVaScRiPt:alert(1)">x</a>`,
		`<a href=" javascript:alert(1)">x</a>`,
		`<a href="java&#115;cript:alert(1)">x</a>`,
		`<a href="vbscript:msgbox(1)">x</a>`,
		`<a href="data:text/html,<script>alert(1)</script>">x</a>`,
	} {
		out := string(Sanitize(in).HTML)
		if strings.Contains(strings.ToLower(out), "javascript") || strings.Contains(strings.ToLower(out), "vbscript") || strings.Contains(strings.ToLower(out), "data:") {
			t.Fatalf("an executable link survived: %q", out)
		}
		if strings.Contains(out, "href=") {
			t.Fatalf("an unsafe link kept its destination: %q", out)
		}
	}
	// A real link keeps its destination and is hardened.
	out := string(Sanitize(`<a href="https://support.acme.example" target="_self" rel="nofollow">Support</a>`).HTML)
	if !strings.Contains(out, `href="https://support.acme.example"`) {
		t.Fatalf("a safe link lost its destination: %s", out)
	}
	if !strings.Contains(out, `target="_blank"`) || !strings.Contains(out, `rel="noopener noreferrer"`) {
		t.Fatalf("a safe link is not hardened: %s", out)
	}
	if strings.Contains(out, "_self") || strings.Contains(out, "nofollow") {
		t.Fatalf("a configured link hardening survived: %s", out)
	}
	// A relative destination stays relative.
	out = string(Sanitize(`<a href="/support">Support</a>`).HTML)
	if !strings.Contains(out, `href="/support"`) || !strings.Contains(out, `rel="noopener noreferrer"`) {
		t.Fatalf("a relative link is not kept and hardened: %s", out)
	}
}

// An image keeps a safe source, a bounded size and a bounded accessible
// name. Nothing else about it survives.
func TestSanitizeImageBounds(t *testing.T) {
	out := string(Sanitize(`<img src="https://cdn.acme.example/logo.png" alt="Acme logo" width="120" height="40">`).HTML)
	if !strings.Contains(out, `src="https://cdn.acme.example/logo.png"`) || !strings.Contains(out, `alt="Acme logo"`) {
		t.Fatalf("a safe image lost its source or name: %s", out)
	}
	if !strings.Contains(out, `width="120"`) || !strings.Contains(out, `height="40"`) {
		t.Fatalf("a safe image lost its size: %s", out)
	}
	// An oversized or malformed dimension is dropped, the image is not.
	out = string(Sanitize(`<img src="https://cdn.acme.example/logo.png" width="9999" height="0">`).HTML)
	if strings.Contains(out, "width=") || strings.Contains(out, "height=") {
		t.Fatalf("an out-of-range dimension survived: %s", out)
	}
	out = string(Sanitize(`<img src="https://cdn.acme.example/logo.png" width="120px">`).HTML)
	if strings.Contains(out, "width=") {
		t.Fatalf("a malformed dimension survived: %s", out)
	}
	// A data source is not a safe source: the image is dropped entirely.
	out = string(Sanitize(`<img src="data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=">`).HTML)
	if out != "" {
		t.Fatalf("a data image survived: %s", out)
	}
	// A long accessible name is truncated, not dropped.
	long := strings.Repeat("a", MaxAttributeRunes+50)
	out = string(Sanitize(`<img src="https://cdn.acme.example/logo.png" alt="` + long + `">`).HTML)
	if !strings.Contains(out, `alt="`+strings.Repeat("a", MaxAttributeRunes)+`"`) {
		t.Fatalf("a long accessible name was not truncated: %s", out)
	}
	if strings.Contains(out, strings.Repeat("a", MaxAttributeRunes+1)) {
		t.Fatalf("a long accessible name was not bounded: %s", out)
	}
	// A relative source stays relative and is served by the console itself.
	out = string(Sanitize(`<img src="/static/logo.png" alt="Acme logo">`).HTML)
	if !strings.Contains(out, `src="/static/logo.png"`) {
		t.Fatalf("a relative image source was dropped: %s", out)
	}
}

// The external image hosts are reported so the page policy can name exactly
// them and nothing else.
func TestSanitizeReportsExternalImageHosts(t *testing.T) {
	r := Sanitize(`<img src="https://cdn.acme.example/logo.png" alt="Acme"><img src="http://legacy.acme.example/logo.png" alt="Legacy"><img src="/static/logo.png" alt="Local">`)
	if len(r.ExternalImageHosts) != 2 {
		t.Fatalf("unexpected image hosts: %v", r.ExternalImageHosts)
	}
	if r.ExternalImageHosts[0] != "http://legacy.acme.example" || r.ExternalImageHosts[1] != "https://cdn.acme.example" {
		t.Fatalf("unexpected image hosts: %v", r.ExternalImageHosts)
	}
	// A link is a navigation, not a subresource: it never opens the image
	// source.
	r = Sanitize(`<a href="https://elsewhere.acme.example/">Elsewhere</a>`)
	if len(r.ExternalImageHosts) != 0 {
		t.Fatalf("a link opened an image source: %v", r.ExternalImageHosts)
	}
}

// An image source that the console cannot name exactly in the page policy
// is not projected at all: the reference is dropped and the host it names
// is never opened.
//
// The host of a source is projected into the page's
// Content-Security-Policy, so a host that carries a policy metacharacter,
// credentials or an unusable port would let the configured value write
// policy directives of its own.
func TestSanitizeRejectsUnsafeImageOrigins(t *testing.T) {
	for _, src := range []string{
		"https://cdn.example;script-src*",
		"https://cdn.example;script-src 'self'",
		"https://cdn.example;script-src'unsafe-inline'https://evil.example",
		"https://*.cdn.example/logo.png",
		"https://cdn.example*/logo.png",
		"https://user:pass@cdn.example/logo.png",
		"https://user@cdn.example/logo.png",
		"https://cdn.example:99999/logo.png",
		"https://cdn.example:65536/logo.png",
		"https://[2001:db8::1]:/logo.png",
		"https://cdn.example:0/logo.png",
		"https://cdn.example:/logo.png",
		"https://cdn.example:8o80/logo.png",
		"https://cdn example/logo.png",
		"https://'cdn.example'/logo.png",
		`https://"cdn.example"/logo.png`,
		"https://cdn.example./logo.png",
		"https://-cdn.example/logo.png",
		"https://cdn..example/logo.png",
		"https://bücher.example/logo.png",
		"https://cdn.example/logo.png\n;script-src 'self'",
		"https://cdn.example/logo.png\t;script-src",
	} {
		r := Sanitize(imageHTML(src))
		if r.HTML != "" {
			t.Fatalf("an unsafe image source survived: %q in %q", src, r.HTML)
		}
		if len(r.ExternalImageHosts) != 0 {
			t.Fatalf("an unsafe image source opened a host: %v for %q", r.ExternalImageHosts, src)
		}
	}
}

// A safe image source keeps its reference exactly and opens exactly one
// policy source.
func TestSanitizeImageOriginsAreValidated(t *testing.T) {
	for _, c := range []struct{ src, rendered, origin string }{
		{"https://cdn.acme.example/logo.png", "https://cdn.acme.example/logo.png", "https://cdn.acme.example"},
		{"http://legacy.acme.example/logo.png", "http://legacy.acme.example/logo.png", "http://legacy.acme.example"},
		{"https://cdn.acme.example:8443/logo.png", "https://cdn.acme.example:8443/logo.png", "https://cdn.acme.example:8443"},
		{"https://192.0.2.10/logo.png", "https://192.0.2.10/logo.png", "https://192.0.2.10"},
		{"https://[2001:db8::1]:8443/logo.png", "https://[2001:db8::1]:8443/logo.png", "https://[2001:db8::1]:8443"},
		{"HTTPS://cdn.acme.example/logo.png", "https://cdn.acme.example/logo.png", "https://cdn.acme.example"},
		{"https://CDN.Acme.Example/logo.png", "https://CDN.Acme.Example/logo.png", "https://cdn.acme.example"},
	} {
		r := Sanitize(imageHTML(c.src))
		if !strings.Contains(string(r.HTML), `src="`+c.rendered+`"`) {
			t.Fatalf("a safe image lost its source: %q in %q", c.src, r.HTML)
		}
		if len(r.ExternalImageHosts) != 1 || r.ExternalImageHosts[0] != c.origin {
			t.Fatalf("unexpected image host for %q: %v", c.src, r.ExternalImageHosts)
		}
	}
}

// A scheme-relative image is normalized to https: the reference the
// browser requests and the origin the policy opens are the same one, on a
// console served over http and over https alike.
func TestSanitizeSchemeRelativeImageBecomesHTTPS(t *testing.T) {
	r := Sanitize(`<img src="//cdn.acme.example/logo.png" alt="Acme logo">`)
	if !strings.Contains(string(r.HTML), `src="https://cdn.acme.example/logo.png"`) {
		t.Fatalf("a scheme-relative image was not normalized: %q", r.HTML)
	}
	if strings.Contains(string(r.HTML), `src="//`) {
		t.Fatalf("a scheme-relative image survived: %q", r.HTML)
	}
	if len(r.ExternalImageHosts) != 1 || r.ExternalImageHosts[0] != "https://cdn.acme.example" {
		t.Fatalf("unexpected image hosts: %v", r.ExternalImageHosts)
	}
}

// An image without a usable source is no content: a footer that holds
// nothing else is no footer at all.
func TestSanitizeImageWithoutSourceIsNoFooter(t *testing.T) {
	for _, in := range []string{
		`<img alt="Acme logo">`,
		`<img src="" alt="Acme logo">`,
		`<img src="   " alt="Acme logo">`,
		`<img src="data:image/png;base64,iVBORw0KGgo=" alt="Acme logo">`,
		`<img src="https://cdn.example;script-src*" alt="Acme logo">`,
		`<img src="https://cdn.example:99999/logo.png" alt="Acme logo">`,
	} {
		r := Sanitize(in)
		if r.HTML != "" {
			t.Fatalf("a footer with no usable image survived: %q for %q", r.HTML, in)
		}
		if len(r.ExternalImageHosts) != 0 {
			t.Fatalf("a footer with no usable image opened a host: %v", r.ExternalImageHosts)
		}
	}
}

// Every policy source the footer reports is a plain host source: no
// wildcard, no directive separator, no quoted token and no whitespace.
func TestSanitizePolicySourcesArePlain(t *testing.T) {
	r := Sanitize(`<img src="https://cdn.acme.example/logo.png" alt="a">` +
		`<img src="http://legacy.acme.example/logo.png" alt="b">` +
		`<img src="//third.acme.example/logo.png" alt="c">` +
		`<img src="/static/logo.png" alt="d">`)
	if len(r.ExternalImageHosts) != 3 {
		t.Fatalf("unexpected image hosts: %v", r.ExternalImageHosts)
	}
	for _, origin := range r.ExternalImageHosts {
		for _, bad := range []string{";", "'", "\"", " ", "\t", "\n", "\r", "*"} {
			if strings.Contains(origin, bad) {
				t.Fatalf("a policy source carries %q: %q", bad, origin)
			}
		}
	}
}

// The accessible name of a link is bounded like every other one, whether
// the link carries a destination or not.
func TestSanitizeLinkTitleIsBounded(t *testing.T) {
	long := strings.Repeat("t", MaxAttributeRunes+50)
	bounded := `title="` + strings.Repeat("t", MaxAttributeRunes) + `"`
	for _, in := range []string{
		`<a href="https://support.acme.example" title="` + long + `">Support</a>`,
		`<a href="/support" title="` + long + `">Support</a>`,
		`<a title="` + long + `">Support</a>`,
	} {
		out := string(Sanitize(in).HTML)
		if !strings.Contains(out, bounded) {
			t.Fatalf("a long link title was not truncated: %s", out)
		}
		if strings.Contains(out, strings.Repeat("t", MaxAttributeRunes+1)) {
			t.Fatalf("a long link title was not bounded: %s", out)
		}
	}
}

// An empty, whitespace-only or fully removed value is no footer at all: the
// page keeps no empty placeholder.
func TestSanitizeEmptyValueIsNoFooter(t *testing.T) {
	for _, in := range []string{"", "   \n\t  ", "<script>alert(1)</script>", "<iframe src=\"https://evil.example\"></iframe>", "<p></p>"} {
		r := Sanitize(in)
		if r.HTML != "" {
			t.Fatalf("an empty footer survived for %q: %q", in, r.HTML)
		}
		if len(r.ExternalImageHosts) != 0 {
			t.Fatalf("an empty footer reported image hosts: %v", r.ExternalImageHosts)
		}
	}
}

// Plain text stays text: it is escaped, never interpreted.
func TestSanitizePlainTextStaysText(t *testing.T) {
	out := string(Sanitize("Support: platform-team@example.org").HTML)
	if out != "Support: platform-team@example.org" {
		t.Fatalf("plain text changed: %q", out)
	}
	out = string(Sanitize(`A & B <not a tag>`).HTML)
	if strings.Contains(out, "<not a tag>") {
		t.Fatalf("plain text became markup: %q", out)
	}
	if !strings.Contains(out, "&amp;") {
		t.Fatalf("plain text was not escaped: %q", out)
	}
}

// Malformed markup is reduced to something safe instead of failing the
// page.
func TestSanitizeMalformedMarkupIsSafe(t *testing.T) {
	for _, in := range []string{
		`<p>unclosed <b>bold`,
		`<img src="https://cdn.acme.example/l.png" alt="x"`,
		`<a href="https://support.acme.example">unclosed`,
		`<div><span>mixed`,
		`<p>text</p></div></span>`,
	} {
		out := string(Sanitize(in).HTML)
		if strings.Contains(strings.ToLower(out), "<script") || strings.Contains(strings.ToLower(out), "onerror") {
			t.Fatalf("malformed markup produced something unsafe: %q", out)
		}
	}
}

// The bound is counted in runes, not bytes, and is enforced by the renderer
// as well as by the configuration loader.
func TestSanitizeRefusesOversizedValue(t *testing.T) {
	if r := Sanitize(strings.Repeat("x", MaxRunes)); r.HTML == "" {
		t.Fatal("a value at the bound was dropped")
	}
	if r := Sanitize(strings.Repeat("x", MaxRunes+1)); r.HTML != "" {
		t.Fatalf("an oversized value survived: %q", r.HTML)
	}
	// The bound counts runes: a multi-byte value of the same rune count is
	// accepted.
	if r := Sanitize(strings.Repeat("é", MaxRunes)); r.HTML == "" {
		t.Fatal("a rune-counted value at the bound was dropped")
	}
}

// Every page render sanitizes the footer, so the sanitizer must be safe for
// concurrent use.
func TestSanitizeIsConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r := Sanitize(`<p>Acme</p><a href="https://support.acme.example">Support</a><img src="https://cdn.acme.example/logo.png" alt="Acme logo">`)
				if !strings.Contains(string(r.HTML), "Acme") {
					t.Error("the sanitized footer lost its content")
					return
				}
				if len(r.ExternalImageHosts) != 1 {
					t.Errorf("the sanitized footer lost its image host: %v", r.ExternalImageHosts)
					return
				}
			}
		}()
	}
	wg.Wait()
}
