// Package footer renders the deployment-configured console footer.
//
// The footer is site configuration, not application code: a deployment may
// want a company logo, a link to its support portal and a short branding
// line. The configured value is therefore treated as untrusted markup.
//
// It is reduced to a small allowlist of elements and attributes by a
// maintained HTML sanitizer (github.com/microcosm-cc/bluemonday) before
// anything is projected into the page, and the sanitized result is the only
// thing the page template ever sees. The raw configured value is never
// rendered and never cast to a trusted type.
//
// The package never fetches a remote resource, never grants a capability and
// never emits script: an image reference stays a reference that the
// operator's browser may or may not load.
//
// The host of an image source is projected into the page's
// Content-Security-Policy, so a source the console cannot name exactly in
// that policy is never rendered: a host that carries a policy metacharacter,
// credentials or an unusable port would let the configured value write
// policy directives of its own. Such a source is dropped, and the host it
// names is never opened.
//
// A scheme-relative image reference is normalized to https, so the
// reference the browser requests and the origin the policy opens are the
// same one whether the console is served over http or over https.
package footer

import (
	"html/template"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MaxRunes is the maximum length of a configured footer value, counted in
// runes (Unicode code points) rather than bytes. A footer is a short
// branding block; the bound keeps an accidental paste of a document out of
// the rendered page and out of the sanitizer's work. The configuration
// loader refuses a longer value and the renderer refuses it again, so the
// two bounds can never disagree.
const MaxRunes = 16384

// MaxAttributeRunes is the longest accessible name (alt, title) a configured
// element may carry. A longer name is truncated rather than dropped, so the
// image keeps a usable description.
const MaxAttributeRunes = 200

// MaxImageDimension is the largest width or height, in pixels, a configured
// image may declare. A malformed or larger dimension is dropped.
const MaxImageDimension = 1024

// Result is the sanitized outcome for one configured footer value.
type Result struct {
	// HTML is the sanitized markup. It is empty when the configured value
	// is empty, whitespace only, or contained nothing that survives the
	// allowlist; the page then renders no footer element at all.
	HTML template.HTML

	// ExternalImageHosts lists the policy sources of the external images
	// the sanitized markup references ("https://cdn.example.org").
	//
	// Every entry is a validated source: a scheme, a host and an optional
	// port, with no policy metacharacter in it. The page's
	// Content-Security-Policy names exactly these sources, so a deployment
	// opens the image host it configured and nothing else. A source the
	// console cannot validate is not named at all, and the image that
	// carried it is dropped.
	ExternalImageHosts []string
}

// Sanitize reduces a configured footer value to the markup the page may
// render. It fails closed: an empty result means the page shows no footer,
// never that it shows something unexpected.
func Sanitize(raw string) Result {
	if utf8.RuneCountInString(raw) > MaxRunes {
		// The configuration loader refuses such a value. A footer that
		// reached the renderer anyway is dropped rather than truncated,
		// because a truncated fragment is not a valid document.
		return Result{}
	}
	nodes, err := html.ParseFragment(strings.NewReader(footerPolicy.Sanitize(raw)), fragmentContext())
	if err != nil {
		return Result{}
	}
	hosts := map[string]bool{}
	kept, content := rewrite(nodes, hosts)
	out := strings.TrimSpace(render(kept))
	if out == "" || !content {
		// Nothing survived that a reader could see: the page renders no
		// footer element at all.
		return Result{}
	}
	return Result{HTML: template.HTML(out), ExternalImageHosts: sorted(hosts)}
}

// footerPolicy is the allowlist for the configured footer.
//
// The list is deliberately small and formatting-only: basic text markup,
// lists, links and images. Everything else is removed from the value:
// script, style, iframe, object, embed and form elements with their
// content, event handlers, inline CSS, raw SVG, and every URL scheme other
// than http, https and a relative path.
var footerPolicy = newPolicy()

func newPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "span", "div", "br", "strong", "em", "b", "i", "ul", "ol", "li", "a", "img")
	// A link carries a destination and an optional accessible name.
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("title").OnElements("a")
	// An image carries a source, an accessible name and bounded dimensions.
	p.AllowAttrs("src").OnElements("img")
	p.AllowAttrs("alt").OnElements("img")
	p.AllowAttrs("title").OnElements("img")
	p.AllowAttrs("width", "height").Matching(dimensionValue).OnElements("img")
	// The link hardening below is applied by the renderer; the allowlist
	// carries the same two values so a second sanitizing pass keeps them.
	p.AllowAttrs("target").Matching(targetValue).OnElements("a")
	p.AllowAttrs("rel").Matching(relValue).OnElements("a")
	// Only http(s) and relative paths survive: javascript:, data: and every
	// other scheme are dropped with the attribute that carried them.
	p.AllowURLSchemes("http", "https")
	p.AllowRelativeURLs(true)
	return p
}

var (
	// dimensionValue accepts a plain decimal pixel count only.
	dimensionValue = regexp.MustCompile(`^[0-9]{1,4}$`)
	// targetValue accepts the single target the renderer sets.
	targetValue = regexp.MustCompile(`^_blank$`)
	// relValue accepts the single relation the renderer sets.
	relValue = regexp.MustCompile(`^noopener noreferrer$`)
)

// fragmentContext is the parsing context for the footer fragment. The footer
// is a fragment of one page: it never gains document structure the operator
// did not write.
func fragmentContext() *html.Node {
	return &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
}

// rewrite applies the renderer-side hardening to the sanitized fragment. It
// can only remove an attribute, remove an element or set one of two fixed
// values, so it cannot widen what the sanitizer allowed.
//
// It returns the nodes that survive and whether the fragment carries
// anything a reader could see: text, or an image with a usable source. A
// fragment without content is no footer.
func rewrite(nodes []*html.Node, hosts map[string]bool) ([]*html.Node, bool) {
	kept := make([]*html.Node, 0, len(nodes))
	content := false
	for _, n := range nodes {
		drop := false
		switch n.Type {
		case html.TextNode:
			if strings.TrimSpace(n.Data) != "" {
				content = true
			}
		case html.ElementNode:
			switch n.Data {
			case "a":
				hardenLink(n)
			case "img":
				if hardenImage(n, hosts) {
					content = true
				} else {
					// An image without a usable source shows nothing.
					// It is removed instead of being left behind as
					// an empty element, and it is not content: a
					// footer that holds nothing else is no footer.
					drop = true
				}
			}
		}
		if drop {
			continue
		}
		kids := childNodes(n)
		keptKids, kidContent := rewrite(kids, hosts)
		if kidContent {
			content = true
		}
		if len(keptKids) != len(kids) {
			replaceChildren(n, keptKids)
		}
		kept = append(kept, n)
	}
	return kept, content
}

// hardenLink keeps every footer link a safe external navigation: it opens in
// a new tab and the opened page gets neither a handle on the console window
// nor a referrer. The accessible name of the link is bounded like every
// other one.
func hardenLink(n *html.Node) {
	truncate(n, "title", MaxAttributeRunes)
	href, ok := attribute(n, "href")
	if !ok {
		return
	}
	u, err := url.Parse(href)
	if err != nil || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") {
		removeAttribute(n, "href")
		return
	}
	setAttribute(n, "target", "_blank")
	setAttribute(n, "rel", "noopener noreferrer")
}

// hardenImage keeps the image reference a plain reference: a safe scheme, a
// validated host, a bounded size and a bounded accessible name. It reports
// whether the image is usable: an image without a usable source is removed
// from the footer rather than rendered as an empty element.
//
// The browser, not the console, resolves and requests the image.
func hardenImage(n *html.Node, hosts map[string]bool) bool {
	src, ok := attribute(n, "src")
	if !ok {
		return false
	}
	ref, ok := safeImageSource(src)
	if !ok {
		removeAttribute(n, "src")
		return false
	}
	if ref.URL != src {
		setAttribute(n, "src", ref.URL)
	}
	if ref.Origin != "" {
		hosts[ref.Origin] = true
	}
	for _, name := range []string{"width", "height"} {
		if v, ok := attribute(n, name); ok {
			d, err := strconv.Atoi(v)
			if err != nil || d <= 0 || d > MaxImageDimension {
				removeAttribute(n, name)
			}
		}
	}
	truncate(n, "alt", MaxAttributeRunes)
	truncate(n, "title", MaxAttributeRunes)
	return true
}

// imageSource is a validated image reference.
type imageSource struct {
	// URL is what the browser requests: the absolute URL of an external
	// image, or the relative path of a same-origin one.
	URL string
	// Origin is the page policy source for the reference. It is empty for
	// a relative reference, which 'self' already covers.
	Origin string
}

// safeImageSource validates an image source and returns the reference the
// page may project.
//
// The validation is strict, because the host of the reference is projected
// into the page's Content-Security-Policy: a host that carries a policy
// metacharacter, credentials or an unusable port would let the configured
// value write policy directives of its own. A source the console does not
// understand is no source at all.
func safeImageSource(src string) (imageSource, bool) {
	if src == "" || strings.TrimSpace(src) != src {
		return imageSource{}, false
	}
	// A control character or a space is never part of a reference the
	// console is willing to project.
	for _, r := range src {
		if r <= ' ' || r == 0x7f {
			return imageSource{}, false
		}
	}
	u, err := url.Parse(src)
	if err != nil {
		return imageSource{}, false
	}
	switch u.Scheme {
	case "":
		if u.Host != "" {
			// A scheme-relative reference inherits the scheme of the
			// page, which the console does not control. It is normalized
			// to https so the reference and the policy name one origin,
			// on a console served over http and over https alike.
			ref, ok := externalImageSource(u, "https")
			if !ok {
				return imageSource{}, false
			}
			ref.URL = "https:" + src
			return ref, true
		}
		// A relative path is served by the console itself: 'self' already
		// covers it.
		if u.Path == "" || strings.TrimSpace(u.Path) == "" {
			return imageSource{}, false
		}
		// The first segment of a relative path may not carry a colon: that
		// is a scheme the console does not recognize.
		if strings.Contains(firstSegment(u.Path), ":") {
			return imageSource{}, false
		}
		return imageSource{URL: src}, true
	case "http", "https":
		ref, ok := externalImageSource(u, u.Scheme)
		if !ok {
			return imageSource{}, false
		}
		ref.URL = src
		return ref, true
	default:
		return imageSource{}, false
	}
}

// externalImageSource validates the authority of an absolute image URL and
// returns the reference and the policy source it opens.
func externalImageSource(u *url.URL, scheme string) (imageSource, bool) {
	// A URL without an authority ("https:logo.png") names no host.
	if u.Opaque != "" || u.Host == "" {
		return imageSource{}, false
	}
	// Credentials are never projected: the console does not authenticate
	// to an image host.
	if u.User != nil {
		return imageSource{}, false
	}
	host, port, ok := splitAuthority(u.Host)
	if !ok {
		return imageSource{}, false
	}
	origin, ok := safeOrigin(scheme, host, port)
	if !ok {
		return imageSource{}, false
	}
	return imageSource{Origin: origin}, true
}

// splitAuthority splits an authority into its host and its port. It rejects
// an authority the console cannot name exactly in a policy.
func splitAuthority(authority string) (host, port string, ok bool) {
	if authority == "" {
		return "", "", false
	}
	if strings.HasPrefix(authority, "[") {
		// An IP-literal: "[address]" or "[address]:port".
		end := strings.Index(authority, "]")
		if end < 0 {
			return "", "", false
		}
		host = authority[:end+1]
		rest := authority[end+1:]
		switch {
		case rest == "":
		case strings.HasPrefix(rest, ":"):
			port = rest[1:]
			if port == "" {
				return "", "", false
			}
		default:
			return "", "", false
		}
		return host, port, true
	}
	if strings.Count(authority, ":") > 1 {
		// An IPv6 address without brackets is not a valid authority.
		return "", "", false
	}
	if i := strings.Index(authority, ":"); i >= 0 {
		host, port = authority[:i], authority[i+1:]
		if port == "" {
			// A trailing colon names no port.
			return "", "", false
		}
		return host, port, true
	}
	return authority, "", true
}

// safeOrigin builds the page policy source for an image host. It fails
// closed: a host the console cannot name exactly is not named at all.
//
// The source is a plain host source: a scheme, a host and an optional port.
// A wildcard, a directive separator, a quoted token or any whitespace would
// let the configured value write policy directives of its own, so none of
// them may survive.
func safeOrigin(scheme, host, port string) (string, bool) {
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	name, ok := safeHost(host)
	if !ok {
		return "", false
	}
	source := scheme + "://" + name
	if port != "" {
		if !decimalPort(port) {
			return "", false
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return "", false
		}
		source += ":" + strconv.Itoa(p)
	}
	if !plainPolicySource(source) {
		return "", false
	}
	return source, true
}

// safeHost validates the host of an image source and returns the canonical
// name the policy names.
//
// A host is a DNS name, an IPv4 address or a bracketed IPv6 address. A name
// is written in ASCII letters, digits, hyphens and dots, and every label of
// it is a valid label. A name in any other script is refused: the policy
// names the ASCII form of a host, and a console that cannot write that form
// exactly does not name the host at all.
func safeHost(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	if strings.HasPrefix(host, "[") {
		// An IP-literal is a bracketed IPv6 address.
		inner := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		ip := net.ParseIP(inner)
		if ip == nil || ip.To4() != nil {
			return "", false
		}
		return "[" + inner + "]", true
	}
	name := strings.ToLower(host)
	if net.ParseIP(name) != nil {
		// An IPv4 address.
		return name, true
	}
	if !validDNSName(name) {
		return "", false
	}
	return name, true
}

// validDNSName reports whether a name is a well-formed DNS host name: ASCII
// letters, digits, hyphens and dots, in labels of one to sixty-three
// characters that neither start nor end with a hyphen.
func validDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// plainPolicySource reports whether a value is a plain policy source: only
// the characters a host source may carry. A wildcard, a directive
// separator, a quoted token or any whitespace fails.
func plainPolicySource(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == ':', c == '/', c == '[', c == ']':
		default:
			return false
		}
	}
	return true
}

// decimalPort reports whether a port is written in decimal digits only.
func decimalPort(port string) bool {
	if port == "" {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	return true
}

// firstSegment returns the first path segment of a path.
func firstSegment(path string) string {
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return path
}

func render(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		if err := html.Render(&b, n); err != nil {
			return ""
		}
	}
	return b.String()
}

// childNodes returns the children of a node as a list.
func childNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

// replaceChildren replaces the children of a node with the given list.
func replaceChildren(n *html.Node, list []*html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		c = next
	}
	for _, c := range list {
		n.AppendChild(c)
	}
}

func attribute(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func setAttribute(n *html.Node, name, value string) {
	for i := range n.Attr {
		if strings.EqualFold(n.Attr[i].Key, name) {
			n.Attr[i].Val = value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: name, Val: value})
}

func removeAttribute(n *html.Node, name string) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			continue
		}
		kept = append(kept, a)
	}
	n.Attr = kept
}

func truncate(n *html.Node, name string, max int) {
	v, ok := attribute(n, name)
	if !ok {
		return
	}
	r := []rune(v)
	if len(r) > max {
		setAttribute(n, name, string(r[:max]))
	}
}

func sorted(hosts map[string]bool) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	for h := range hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
