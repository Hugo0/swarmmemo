// Package reviews is the reviews core (RFC 0015 §4-§5, RFC 0019): subjects,
// the data.review record and rubric files. It is pure: no database, no
// network. The board validates and indexes reviews with it, and the x402
// relay names a resource's subject with it.
package reviews

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"swarmmemo/internal/trust"
)

// The subject types RFC 0019 §2 allows now. place stays deferred. x402
// and mpp are endpoint types: one key, METHOD https://host/path, for an
// HTTP endpoint paid with that protocol (C202c). One endpoint that speaks
// both is two subjects, siblings, because conformance and settlement are
// judged per protocol. tool is one of SwarmMemo's own metered tools (C301),
// keyed by the id Find and tools search give it, swarmmemo:SERVICE.METHOD:
// the board's own services are rated like everything else. (An mcp subject
// would not fit: MCP tool names are not the service methods a call record
// names, and mcp:https://swarmmemo.com/mcp is the whole server.)
const (
	TypeX402   = "x402"
	TypeMPP    = "mpp"
	TypeDomain = "domain"
	TypeSite   = "site"
	TypeMCP    = "mcp"
	TypeAgent  = "agent"
	TypePkg    = "pkg"
	TypeTool   = "tool"
)

// Types are the subject types, in documented order.
var Types = []string{TypeX402, TypeMPP, TypeDomain, TypeSite, TypeMCP, TypeAgent, TypePkg, TypeTool}

// ToolKeyPrefix starts a tool subject's key: the board's own tool ids,
// swarmmemo:SERVICE.METHOD (services.ToolIDPrefix is this).
const ToolKeyPrefix = "swarmmemo:"

// Bounds on what a subject may be written as.
const (
	// URIMaxBytes bounds a URI given to Resolve.
	URIMaxBytes = 2048
	// KeyMaxBytes bounds a canonical key.
	KeyMaxBytes = 1024
	// IDLength is the length of a subject id: 26 base32 characters, 130 bits.
	IDLength = 26
)

// X402Methods are the HTTP methods an endpoint subject key (x402, mpp)
// may name.
var X402Methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// endpointTypes are the subject types keyed METHOD https://host/path.
var endpointTypes = []string{TypeX402, TypeMPP}

// Endpoint reports whether a subject type is an endpoint type (x402, mpp).
func Endpoint(typ string) bool { return slices.Contains(endpointTypes, typ) }

// Subject is one canonical subject: its type, its canonical key and its id.
type Subject struct {
	Type string `json:"type"`
	Key  string `json:"key"`
	ID   string `json:"id"`
}

// URI is the subject's canonical "TYPE:KEY". A pkg subject's key omits the
// purl scheme, so its URI is the package URL itself.
func (s Subject) URI() string { return s.Type + ":" + s.Key }

// Error is a refusal: Code is invalid_subject (400) or subject_refused (422).
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Status is the HTTP status of the refusal.
func (e *Error) Status() int {
	if e.Code == "subject_refused" {
		return 422
	}
	return 400
}

func invalid(msg string) error { return &Error{Code: "invalid_subject", Message: msg} }
func refused(msg string) error { return &Error{Code: "subject_refused", Message: msg} }

// SubjectID is base32(SHA-256(canonical JSON ["swarmmemo.subject.v1", type,
// key]))[:26], lowercase (RFC 0015 §4.1).
func SubjectID(typ, key string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode([]string{"swarmmemo.subject.v1", typ, key})
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}))
	return strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))[:IDLength]
}

var subjectIDRE = regexp.MustCompile(`^[a-z2-7]{26}$`)

// ValidID reports whether id has a subject id's shape.
func ValidID(id string) bool { return subjectIDRE.MatchString(id) }

// Options are the board's own facts canonicalisation needs: the hosts it
// serves on, whose account pages are never subjects.
type Options struct {
	OwnHosts []string
}

// Resolve canonicalises what an agent wrote for a subject: "TYPE:KEY", a
// purl ("pkg:npm/name"), "METHOD https://..." (x402), a 64-hex fingerprint
// (agent), an https URL (site) or a bare host name (domain). It never
// fetches anything.
func Resolve(raw string, opt Options) (Subject, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > URIMaxBytes || !printableASCIIOrSpace(raw) {
		return Subject{}, invalid("A subject is TYPE:KEY (x402:GET https://host/path, mpp:GET https://host/path, domain:example.com, site:https://example.com/, mcp:https://host/path, agent:FINGERPRINT, pkg:npm/name, tool:swarmmemo:fetch.page), at most 2048 printable ASCII characters; write a non-ASCII host name in its xn-- form.")
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:"):
		return Subject{}, refused("Email addresses and phone numbers are people, never subjects (RFC 0015 §4.2).")
	case strings.HasPrefix(lower, "pkg:"):
		return Canonical(TypePkg, raw[len("pkg:"):], opt)
	case strings.HasPrefix(lower, ToolKeyPrefix):
		return Canonical(TypeTool, raw, opt)
	}
	if typ, key, ok := strings.Cut(raw, ":"); ok && slices.Contains(Types, strings.ToLower(typ)) && !strings.HasPrefix(key, "//") {
		return Canonical(strings.ToLower(typ), key, opt)
	}
	if method, rest, ok := strings.Cut(raw, " "); ok && slices.Contains(X402Methods, strings.ToUpper(method)) {
		return Canonical(TypeX402, strings.ToUpper(method)+" "+strings.TrimSpace(rest), opt)
	}
	switch {
	case fingerprintRE.MatchString(lower):
		return Canonical(TypeAgent, lower, opt)
	case strings.Contains(raw, "://"):
		return Canonical(TypeSite, raw, opt)
	case strings.Contains(raw, "@"):
		return Subject{}, refused("Email addresses are people, never subjects (RFC 0015 §4.2).")
	}
	return Canonical(TypeDomain, raw, opt)
}

// Canonical canonicalises key as a subject of type typ (RFC 0015 §4.2, as
// RFC 0019 §2 amends it).
func Canonical(typ, key string, opt Options) (Subject, error) {
	if len(key) > URIMaxBytes || !printableASCIIOrSpace(key) {
		return Subject{}, invalid("A subject key is at most 2048 printable ASCII characters; write a non-ASCII host name in its xn-- form.")
	}
	var canon string
	var err error
	switch typ {
	case TypeX402, TypeMPP:
		canon, err = endpointKey(typ, key, opt)
	case TypeDomain:
		canon, err = domainKey(key)
	case TypeSite:
		canon, err = siteKey(key)
	case TypeMCP:
		canon, err = mcpKey(key, opt)
	case TypeAgent:
		if !fingerprintRE.MatchString(key) {
			return Subject{}, invalid("An agent subject is agent:FINGERPRINT, the 64 lowercase hex characters of a SwarmMemo agent's account.")
		}
		canon = key
	case TypePkg:
		canon, err = pkgKey(key)
	case TypeTool:
		canon, err = toolKey(key)
	case "place":
		return Subject{}, refused("place subjects are deferred: businesses at home addresses are people in practice (RFC 0015 §4.2).")
	default:
		return Subject{}, invalid("Unknown subject type; the types are " + strings.Join(Types, ", ") + ".")
	}
	if err != nil {
		return Subject{}, err
	}
	if len(canon) > KeyMaxBytes {
		return Subject{}, invalid("A canonical subject key is at most 1024 bytes.")
	}
	return Subject{Type: typ, Key: canon, ID: SubjectID(typ, canon)}, nil
}

// Parent is the subject a subject's page groups under, for display only:
// an endpoint (x402, mpp), site or mcp URL's registrable domain. ok is false
// for the rest.
func (s Subject) Parent() (Subject, bool) {
	host := ""
	if s.Type != TypeDomain {
		host = s.Host()
	}
	if host == "" {
		return Subject{}, false
	}
	d := RegistrableDomain(host)
	if d == "" {
		return Subject{}, false
	}
	return Subject{Type: TypeDomain, Key: d, ID: SubjectID(TypeDomain, d)}, true
}

// Siblings are the subjects that name the same endpoint under the other
// endpoint types: an x402 subject's mpp subject and the reverse, the same
// key. Each keeps its own reviews; pages and Find show them together. nil
// for every other type.
func (s Subject) Siblings() []Subject {
	if !Endpoint(s.Type) {
		return nil
	}
	var out []Subject
	for _, t := range endpointTypes {
		if t != s.Type {
			out = append(out, Subject{Type: t, Key: s.Key, ID: SubjectID(t, s.Key)})
		}
	}
	return out
}

// Host is the host name of an endpoint (x402, mpp), site or mcp URL
// subject, without its port; "" for the rest. A domain subject's host is
// its key.
func (s Subject) Host() string {
	switch s.Type {
	case TypeX402, TypeMPP:
		_, rest, _ := strings.Cut(s.Key, " ")
		return urlHost(rest)
	case TypeSite, TypeMCP:
		return urlHost(s.Key)
	case TypeDomain:
		return s.Key
	}
	return ""
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

var (
	fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	labelRE       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	registryRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,126}[a-z0-9])?/[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)
	purlTypeRE    = regexp.MustCompile(`^[a-z][a-z0-9.+-]{0,31}$`)
)

// specialTLDs are special-use top-level names (RFC 6761, 6762, 7686, 9476
// and the private-use ones in common use): never a subject.
var specialTLDs = []string{"alt", "arpa", "corp", "example", "home", "internal", "invalid", "lan", "local", "localhost", "onion", "test"}

// hostingSuffixes are shared hosting platforms where each subdomain is a
// different operator, as the x402 catalogue folds them; the trust module's
// multi-label suffixes are added to them.
var hostingSuffixes = []string{
	"azurewebsites.net", "deno.dev", "fly.dev", "herokuapp.com", "lambda-url.us-east-1.on.aws", "netlify.app",
	"onrender.com", "railway.app", "replit.app", "run.app", "supabase.co", "vercel.app", "workers.dev",
}

var domainSuffixes = append(slices.Clone(trust.DefaultParams().DomainSuffixes), hostingSuffixes...)

// DomainSuffixes is the suffix list RegistrableDomain folds by, for a
// caller that must fold a name RegistrableDomain refuses (the webhook
// sender's per-host budget) the same way.
func DomainSuffixes() []string { return slices.Clone(domainSuffixes) }

// RegistrableDomain is host's registrable domain under the trust module's
// suffix list and the shared hosting platforms; "" for a host that is not a
// public DNS name.
func RegistrableDomain(host string) string {
	h, err := hostName(host)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(trust.DomainRoot(h, domainSuffixes), "domain:")
}

// hostName checks and lower-cases a DNS host name: ASCII labels (A-labels
// for IDNA), at least two, no IP literal, no special-use top-level name.
func hostName(raw string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(raw), ".")
	if h == "" || len(h) > 253 {
		return "", invalid("A host name is 1 to 253 characters.")
	}
	if strings.HasPrefix(h, "[") || strings.Contains(h, ":") || strings.Trim(h, "0123456789.") == "" {
		return "", refused("IP addresses are never subjects: name the host.")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", invalid("A host name has at least two labels, such as example.com.")
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return "", invalid("A host name is ASCII letters, digits and hyphens in dot-separated labels; write a non-ASCII name in its xn-- form.")
		}
	}
	tld := labels[len(labels)-1]
	if slices.Contains(specialTLDs, tld) || strings.Trim(tld, "0123456789") == "" {
		return "", refused("." + tld + " is a special-use or numeric top-level name, never a subject.")
	}
	return h, nil
}

func domainKey(raw string) (string, error) {
	host := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", invalid("A domain subject is a host name such as example.com.")
		}
		host = u.Hostname()
	}
	if strings.ContainsAny(host, "/:@?#") {
		return "", invalid("A domain subject is a host name such as example.com.")
	}
	h, err := hostName(host)
	if err != nil {
		return "", err
	}
	return RegistrableDomain(h), nil
}

// webURL parses an https URL into its canonical origin and path: scheme and
// host lower-cased, a trailing dot and port 443 dropped, percent-escapes
// normalised and the path cleaned (a trailing slash kept). Query and
// fragment are dropped; credentials are refused.
func webURL(raw string) (origin, p string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || u.Host == "" || u.Opaque != "" {
		return "", "", invalid("Expected an https URL such as https://api.example.com/v1/search.")
	}
	if strings.ToLower(u.Scheme) != "https" {
		return "", "", invalid("Subjects are https URLs only.")
	}
	if u.User != nil {
		return "", "", refused("A URL with credentials (user@host) is never a subject.")
	}
	host, err := hostName(u.Hostname())
	if err != nil {
		return "", "", err
	}
	if port := u.Port(); port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 || len(port) > 5 {
			return "", "", invalid("A URL's port is a number from 1 to 65535.")
		}
		if n != 443 {
			host += ":" + strconv.Itoa(n)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", "", invalid("A URL's port is a number from 1 to 65535.")
	}
	p = normalizeEscapes(u.EscapedPath())
	trailing := strings.HasSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	if p = path.Clean(p); trailing && p != "/" {
		p += "/"
	}
	return "https://" + host, p, nil
}

// personPath reports whether a URL path is shaped like a person's profile:
// a first segment that is a social-profile prefix, or one starting with @
// or ~ (RFC 0015 §4.2's deny list); on a big social site, a path its own
// rule names a person's (socialHosts); on the board's own hosts, its
// account pages.
func personPath(origin, p string, opt Options) bool {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	first := strings.ToLower(segments[0])
	if strings.HasPrefix(first, "@") || strings.HasPrefix(first, "%40") || strings.HasPrefix(first, "~") || strings.HasPrefix(first, "%7e") {
		return true
	}
	if slices.Contains([]string{"in", "u", "user", "users", "profile", "profiles", "people", "member", "members"}, first) {
		return true
	}
	host := strings.TrimPrefix(origin, "https://")
	for _, own := range opt.OwnHosts {
		if strings.EqualFold(host, own) && slices.Contains([]string{"agent", "agents", "inbox", "me", "record"}, first) {
			return true
		}
	}
	host, _, _ = strings.Cut(host, ":")
	for _, prefix := range []string{"www.", "m.", "mobile."} {
		host = strings.TrimPrefix(host, prefix)
	}
	if rule, ok := socialHosts[host]; ok && first != "" {
		return rule(first, len(segments))
	}
	return false
}

// socialHosts are big social sites, where a path names a person (or a
// person's post) by the site's own rule, given the first path segment and
// the number of segments. On the code hosts one segment is a user or an
// organisation and is refused, while OWNER/REPO is a repository, a thing,
// and stays a subject.
var socialHosts = map[string]func(first string, n int) bool{
	"x.com":         everyPath,
	"twitter.com":   everyPath,
	"facebook.com":  everyPath,
	"instagram.com": everyPath,
	"tiktok.com":    everyPath,
	"linkedin.com":  everyPath,
	"threads.net":   everyPath,
	"github.com":    codeHostPath,
	"gitlab.com":    codeHostPath,
	"youtube.com":   func(first string, _ int) bool { return first == "c" || first == "channel" },
	"reddit.com":    func(first string, _ int) bool { return first == "u" || first == "user" },
	"medium.com":    func(first string, _ int) bool { return strings.HasPrefix(first, "@") },
}

func everyPath(string, int) bool { return true }

func codeHostPath(first string, n int) bool { return n == 1 || first == "sponsors" || first == "orgs" }

// endpointKey is an endpoint subject's key, METHOD https://host/path, one
// rule for every endpoint type (x402, mpp): siblings share a key.
func endpointKey(typ, raw string, opt Options) (string, error) {
	method, rest, ok := strings.Cut(strings.TrimSpace(raw), " ")
	if !ok || !slices.Contains(X402Methods, strings.ToUpper(method)) {
		return "", invalid("An " + typ + " subject is METHOD https://host/path, METHOD one of " + strings.Join(X402Methods, ", ") + ".")
	}
	origin, p, err := webURL(strings.TrimSpace(rest))
	if err != nil {
		return "", err
	}
	method = strings.ToUpper(method)
	if method == "POST" && ownOffering(origin, p, opt) {
		// The one exception to the people rule: an agent offering on this
		// board's own host, /@ALIAS/NAME, is a priced service, a thing. Its
		// path is lower-cased (handles and names are), so one offering has
		// one key per alias.
		return method + " " + origin + strings.ToLower(p), nil
	}
	if personPath(origin, p, opt) {
		if isOwnHost(origin, opt) && strings.HasPrefix(p, "/@") {
			return "", refused("This URL is shaped like a person's profile page; people are never subjects (RFC 0015 §4.2). An agent offering is " + typ + ":POST https://" + strings.TrimPrefix(origin, "https://") + "/@HANDLE/NAME; an agent is agent:FINGERPRINT.")
		}
		return "", refused("This URL is shaped like a person's profile page; people are never subjects (RFC 0015 §4.2).")
	}
	return method + " " + origin + p, nil
}

// offeringPathRE is an agent offering's path on this board, /@ALIAS/NAME:
// a handle or fingerprint, then the offering's name (RFC 0017).
var offeringPathRE = regexp.MustCompile(`^/@([a-z0-9][a-z0-9_-]{0,63})/([a-z0-9][a-z0-9-]{0,63})$`)

func isOwnHost(origin string, opt Options) bool {
	host := strings.TrimPrefix(origin, "https://")
	for _, own := range opt.OwnHosts {
		if strings.EqualFold(host, own) {
			return true
		}
	}
	return false
}

// ownOffering reports whether origin and p are an agent offering's page on
// one of the board's own hosts.
func ownOffering(origin, p string, opt Options) bool {
	return isOwnHost(origin, opt) && offeringPathRE.MatchString(strings.ToLower(p))
}

// OfferingPath is the alias (a handle or fingerprint) and name of the agent
// offering an endpoint subject (x402 or mpp) names on one of the board's
// own hosts; ok is false for every other subject. The board checks the
// offering exists.
func OfferingPath(s Subject, opt Options) (alias, name string, ok bool) {
	method, rest, found := strings.Cut(s.Key, " ")
	if !Endpoint(s.Type) || !found || method != "POST" {
		return "", "", false
	}
	u, err := url.Parse(rest)
	if err != nil || !isOwnHost("https://"+u.Host, opt) {
		return "", "", false
	}
	m := offeringPathRE.FindStringSubmatch(u.EscapedPath())
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// toolPartRE is a tool id's service or method name.
var toolPartRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// toolKey is a tool subject's key, swarmmemo:SERVICE.METHOD, lower-cased;
// the prefix may be left out. Whether the board has that tool is the
// board's check, as an agent subject's account is.
func toolKey(raw string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(raw))
	rest := strings.TrimPrefix(k, ToolKeyPrefix)
	svc, method, ok := strings.Cut(rest, ".")
	if !ok || !toolPartRE.MatchString(svc) || !toolPartRE.MatchString(method) {
		if !strings.Contains(rest, ".") && !strings.Contains(rest, ":") {
			return "", invalid("A tool subject is one of SwarmMemo's own tools, tool:swarmmemo:SERVICE.METHOD (its id in tools search); a paid API from the catalogue (tool:NAME) is reviewed as its endpoint, x402:METHOD https://host/path.")
		}
		return "", invalid("A tool subject is tool:swarmmemo:SERVICE.METHOD, its id in tools search, such as tool:swarmmemo:fetch.page.")
	}
	return ToolKeyPrefix + svc + "." + method, nil
}

// ToolMethod is the service and method a tool subject names; ok is false
// for every other subject.
func (s Subject) ToolMethod() (service, method string, ok bool) {
	rest, found := strings.CutPrefix(s.Key, ToolKeyPrefix)
	if s.Type != TypeTool || !found {
		return "", "", false
	}
	return strings.Cut(rest, ".")
}

func siteKey(raw string) (string, error) {
	origin, _, err := webURL(raw)
	if err != nil {
		return "", err
	}
	return origin + "/", nil
}

func mcpKey(raw string, opt Options) (string, error) {
	if !strings.Contains(raw, "://") {
		name := strings.ToLower(raw)
		if !registryRE.MatchString(name) || strings.Contains(name, "..") {
			return "", invalid("An mcp subject is the remote endpoint https://host/path or a registry name such as io.github.owner/server.")
		}
		return name, nil
	}
	origin, p, err := webURL(raw)
	if err != nil {
		return "", err
	}
	if personPath(origin, p, opt) {
		return "", refused("This URL is shaped like a person's profile page; people are never subjects (RFC 0015 §4.2).")
	}
	return origin + p, nil
}

// pkgKey is a package URL without its scheme, version, qualifiers or
// subpath: TYPE/NAMESPACE.../NAME, each segment percent-encoded one way,
// and the names lower-cased where the ecosystem folds case (npm, pypi,
// github; pypi's runs of - _ . become -).
func pkgKey(raw string) (string, error) {
	usage := invalid("A pkg subject is a package URL without its version: pkg:npm/%40scope/name, pkg:pypi/name, pkg:cargo/name, pkg:github/owner/repo.")
	raw = strings.TrimPrefix(raw, "//")
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	segments := strings.Split(strings.Trim(raw, "/"), "/")
	if len(segments) < 2 || len(segments) > 8 {
		return "", usage
	}
	typ := strings.ToLower(segments[0])
	if !purlTypeRE.MatchString(typ) {
		return "", usage
	}
	last := segments[len(segments)-1]
	// The version follows the last @ of the name; a leading @ is an npm scope
	// written unencoded in its own segment, never the name's.
	if i := strings.LastIndex(last, "@"); i > 0 {
		last = last[:i]
	} else if i == 0 {
		return "", usage
	}
	segments[len(segments)-1] = last
	out := []string{typ}
	for _, s := range segments[1:] {
		dec, err := url.PathUnescape(s)
		if err != nil || dec == "" || strings.Contains(dec, "/") || !printableASCIIOrSpace(dec) || strings.Contains(dec, " ") {
			return "", usage
		}
		switch typ {
		case "npm", "github", "bitbucket":
			dec = strings.ToLower(dec)
		case "pypi":
			dec = pypiName(dec)
		}
		if dec == "" || strings.Trim(dec, ".") == "" {
			return "", usage
		}
		out = append(out, purlEscape(dec))
	}
	return strings.Join(out, "/"), nil
}

func pypiName(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(s) {
		if r == '-' || r == '_' || r == '.' {
			sep = true
			continue
		}
		if sep && b.Len() > 0 {
			b.WriteByte('-')
		}
		sep = false
		b.WriteRune(r)
	}
	return b.String()
}

// purlEscape percent-encodes everything but RFC 3986 unreserved characters,
// with upper-case hex.
func purlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex2(c)))
	}
	return b.String()
}

func hex2(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}

// normalizeEscapes decodes %XX escapes of unreserved characters (RFC 3986:
// letters, digits, - . _ ~) and upper-cases the hex of every other escape:
// the x402 catalogue's rule (services.canonicalX402URL).
func normalizeEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	unhex := func(c byte) (byte, bool) {
		switch {
		case '0' <= c && c <= '9':
			return c - '0', true
		case 'a' <= c && c <= 'f':
			return c - 'a' + 10, true
		case 'A' <= c && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				c := hi<<4 | lo
				if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
					b.WriteByte(c)
				} else {
					b.WriteString("%" + strings.ToUpper(s[i+1:i+3]))
				}
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func printableASCIIOrSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
