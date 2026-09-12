package references

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const cuttleAdapter = "cuttle-worklog-index-v1"
const cuttleOrigin = "https://blog.cuttle.af"
const cuttleFeed = cuttleOrigin + "/content-index.json"

var sourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,255}$`)
var utcPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`)
var legacyDatePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$`)
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
var browserIPv4Tail = regexp.MustCompile(`^(?:[0-9]+|0[xX][0-9a-fA-F]+)$`)

var nonpublicIPRanges = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "240.0.0.0/4", "100.64.0.0/10", "224.0.0.0/4",
	"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
	"2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
)
var publicIPExceptions = prefixes("192.0.0.9/32", "192.0.0.10/32", "2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28", "2001:30::/28")

func prefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, len(values))
	for index, value := range values {
		result[index] = netip.MustParsePrefix(value)
	}
	return result
}

// Fixed cross-runtime profile, not DNS or a claim about future IP assignments.
func publicIP(address netip.Addr) bool {
	address = address.Unmap()
	for _, allowed := range publicIPExceptions {
		if allowed.Contains(address) {
			return true
		}
	}
	for _, denied := range nonpublicIPRanges {
		if denied.Contains(address) {
			return false
		}
	}
	return address.IsValid()
}

type grant struct {
	approved          bool
	reviewed, expires int64
}
type policySource struct {
	id, name, adapter, feed string
	enabled                 bool
	permissions             map[string]grant
}

func object(value any, required, optional string) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, name := range strings.Fields(required) {
		allowed[name] = true
		if _, exists := result[name]; !exists {
			return nil, false
		}
	}
	for _, name := range strings.Fields(optional) {
		allowed[name] = true
	}
	for name := range result {
		if !allowed[name] {
			return nil, false
		}
	}
	return result, true
}
func number(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	result, err := number.Int64()
	return result, err == nil && result >= 0 && result <= maxSafeInteger
}
func text(value any, maximum int) (string, bool) {
	result, ok := value.(string)
	return result, ok && boundedText(result, maximum)
}
func flag(value any) (bool, bool) { result, ok := value.(bool); return result, ok }

func permissionTime(value any) (int64, bool) {
	raw, ok := value.(string)
	if !ok || !utcPattern.MatchString(raw) {
		return 0, false
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z", raw)
	return parsed.Unix(), err == nil && parsed.Year() >= 1
}

func allowedURL(raw string, public bool) bool {
	if !boundedText(raw, 4096) || raw == "" || strings.ContainsAny(raw, "\\#") {
		return false
	}
	for _, character := range raw {
		if character <= 32 || character == 127 {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	if public && (parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "?")) {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.Contains(host, "%") {
		return false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !publicIP(address) {
			return false
		}
	} else {
		labels := strings.Split(host, ".")
		if !hostPattern.MatchString(host) || len(host) > 253 || strings.Contains(host, "..") || browserIPv4Tail.MatchString(labels[len(labels)-1]) {
			return false
		}
	}
	if public {
		path := strings.ToLower(parsed.Path)
		if !boundedText(path, 4096) || strings.Contains(path, "\\") {
			return false
		}
		for _, character := range path {
			if character < 32 || character == 127 {
				return false
			}
		}
		for _, prefix := range []string{"/w/", "/w64/", "/c64/", "/v1/command"} {
			if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
				return false
			}
		}
		for _, segment := range strings.Split(path, "/") {
			if segment == "." || segment == ".." {
				return false
			}
		}
	}
	return true
}

func registry(raw []byte) (map[string]policySource, error) {
	value, err := parseJSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := object(value, "version sources", "max_catalog_bytes")
	if !ok {
		return nil, ErrUnavailable
	}
	version, ok := number(root["version"])
	if !ok || version != 1 {
		return nil, ErrUnavailable
	}
	if maximum, exists := root["max_catalog_bytes"]; exists {
		value, ok := number(maximum)
		if !ok || value < 1<<20 || value > 1<<30 {
			return nil, ErrUnavailable
		}
	}
	rows, ok := root["sources"].([]any)
	if !ok || len(rows) > 50 {
		return nil, ErrUnavailable
	}
	result := map[string]policySource{}
	for _, row := range rows {
		item, ok := object(row, "id adapter enabled", "name feed_url status terms_url permissions limits")
		if !ok {
			return nil, ErrUnavailable
		}
		id, ok := text(item["id"], 64)
		if !ok || !sourcePattern.MatchString(id) {
			return nil, ErrUnavailable
		}
		if _, exists := result[id]; exists {
			return nil, ErrUnavailable
		}
		adapter, ok := text(item["adapter"], 64)
		if !ok || (adapter != cuttleAdapter && adapter != "jsonfeed-1.1") {
			return nil, ErrUnavailable
		}
		enabled, ok := flag(item["enabled"])
		if !ok {
			return nil, ErrUnavailable
		}
		name := ""
		if rawName, exists := item["name"]; exists {
			name, ok = text(rawName, 256)
			if !ok {
				return nil, ErrUnavailable
			}
		}
		feed := ""
		if rawFeed, exists := item["feed_url"]; exists && rawFeed != nil {
			feed, ok = text(rawFeed, 4096)
			if !ok || !allowedURL(feed, false) {
				return nil, ErrUnavailable
			}
		}
		if enabled && feed == "" || adapter == cuttleAdapter && feed != cuttleFeed {
			return nil, ErrUnavailable
		}
		if terms, exists := item["terms_url"]; exists && terms != nil && terms != "" {
			value, ok := text(terms, 4096)
			if !ok || !allowedURL(value, false) {
				return nil, ErrUnavailable
			}
		}
		permissions := map[string]grant{}
		if rawPermissions, exists := item["permissions"]; exists {
			values, ok := object(rawPermissions, "", "automated_collection full_text_storage public_archive hugging_face")
			if !ok {
				return nil, ErrUnavailable
			}
			for name, rawGrant := range values {
				fields, ok := object(rawGrant, "approved", "evidence_url reviewed_at expires_at scope")
				if !ok {
					return nil, ErrUnavailable
				}
				approved, ok := flag(fields["approved"])
				if !ok {
					return nil, ErrUnavailable
				}
				g := grant{approved: approved}
				if approved {
					evidence, ok := text(fields["evidence_url"], 4096)
					if !ok || !allowedURL(evidence, false) {
						return nil, ErrUnavailable
					}
					g.reviewed, ok = permissionTime(fields["reviewed_at"])
					if !ok {
						return nil, ErrUnavailable
					}
					g.expires, ok = permissionTime(fields["expires_at"])
					if !ok || g.reviewed >= g.expires {
						return nil, ErrUnavailable
					}
					scope, ok := text(fields["scope"], 1024)
					if !ok || scope == "" {
						return nil, ErrUnavailable
					}
				}
				if adapter == cuttleAdapter && name == "hugging_face" && approved {
					return nil, ErrUnavailable
				}
				permissions[name] = g
			}
		}
		if rawLimits, exists := item["limits"]; exists {
			limits, ok := object(rawLimits, "", "feed_bytes item_bytes items_per_fetch total_items revisions_per_item")
			if !ok {
				return nil, ErrUnavailable
			}
			maximums := map[string]int64{"feed_bytes": 8 << 20, "item_bytes": 262144, "items_per_fetch": 1000, "total_items": 100000, "revisions_per_item": 100}
			for name, raw := range limits {
				value, ok := number(raw)
				if !ok || value < 1 || value > maximums[name] {
					return nil, ErrUnavailable
				}
			}
		}
		result[id] = policySource{id: id, name: name, adapter: adapter, feed: feed, enabled: enabled, permissions: permissions}
	}
	return result, nil
}

func permitted(source policySource, right string, now int64) (grant, bool) {
	g, exists := source.permissions[right]
	return g, exists && g.approved && g.reviewed <= now && now < g.expires
}

func suppressions(raw []byte) (map[string]bool, error) {
	value, err := canonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := object(value, "version ids", "")
	if !ok {
		return nil, ErrUnavailable
	}
	version, ok := number(root["version"])
	if !ok || version != 1 {
		return nil, ErrUnavailable
	}
	ids, ok := root["ids"].([]any)
	if !ok || len(ids) > 10000 {
		return nil, ErrUnavailable
	}
	result, previous := map[string]bool{}, ""
	for _, rawID := range ids {
		id, ok := text(rawID, 64)
		if !ok || !hashPattern.MatchString(id) || id <= previous {
			return nil, ErrUnavailable
		}
		result[id], previous = true, id
	}
	return result, nil
}

func validate(registryRaw, suppressionRaw, projectionRaw []byte, now int64) (*Snapshot, error) {
	if len(registryRaw) > 1<<20 || len(suppressionRaw) > 1<<20 || len(projectionRaw) > 8<<20 || now < 0 || now > maxSafeInteger-86400 {
		return nil, ErrUnavailable
	}
	policies, err := registry(registryRaw)
	if err != nil {
		return nil, err
	}
	suppressed, err := suppressions(suppressionRaw)
	if err != nil {
		return nil, err
	}
	value, err := canonicalJSON(projectionRaw)
	if err != nil {
		return nil, err
	}
	root, ok := object(value, "version state registry_sha256 suppression_sha256 generated_at valid_until sources references", "")
	if !ok {
		return nil, ErrUnavailable
	}
	version, ok := number(root["version"])
	if !ok || version != 1 || root["state"] != "ready" || root["registry_sha256"] != hash(registryRaw) || root["suppression_sha256"] != hash(suppressionRaw) {
		return nil, ErrUnavailable
	}
	generated, ok := number(root["generated_at"])
	if !ok || generated > now+60 {
		return nil, ErrUnavailable
	}
	notBefore := max(int64(0), generated-60)
	until, ok := number(root["valid_until"])
	if !ok || until <= generated || until > generated+900 || until <= now {
		return nil, ErrUnavailable
	}
	sourceRows, ok := root["sources"].([]any)
	if !ok || len(sourceRows) > 50 {
		return nil, ErrUnavailable
	}
	referenceRows, ok := root["references"].([]any)
	if !ok || len(referenceRows) > 1000 {
		return nil, ErrUnavailable
	}
	sources := map[string]Source{}
	previousSource := ""
	for _, row := range sourceRows {
		item, ok := object(row, "id name feed_url adapter last_attempted_at last_successful_at status attribution_basis", "")
		if !ok {
			return nil, ErrUnavailable
		}
		id, ok := text(item["id"], 64)
		if !ok || !sourcePattern.MatchString(id) || id <= previousSource {
			return nil, ErrUnavailable
		}
		previousSource = id
		policy, exists := policies[id]
		if !exists || !policy.enabled {
			return nil, ErrUnavailable
		}
		name, ok := text(item["name"], 256)
		if !ok || name != policy.name {
			return nil, ErrUnavailable
		}
		feed, ok := text(item["feed_url"], 4096)
		if !ok || feed != policy.feed || !allowedURL(feed, true) || item["adapter"] != policy.adapter {
			return nil, ErrUnavailable
		}
		attempted, ok := number(item["last_attempted_at"])
		if !ok || attempted > generated+60 {
			return nil, ErrUnavailable
		}
		success, ok := number(item["last_successful_at"])
		if !ok || success <= 0 || success > attempted || until > success+86400 {
			return nil, ErrUnavailable
		}
		status, ok := text(item["status"], 32)
		if !ok || status != "ok" && status != "not_modified" {
			return nil, ErrUnavailable
		}
		basis := "source_declared"
		if policy.adapter == cuttleAdapter {
			basis = "site_declared_human_agent_co_creation"
		}
		if item["attribution_basis"] != basis {
			return nil, ErrUnavailable
		}
		for _, right := range []string{"automated_collection", "public_archive"} {
			g, ok := permitted(policy, right, now)
			if !ok || until > g.expires {
				return nil, ErrUnavailable
			}
			notBefore = max(notBefore, g.reviewed)
		}
		sources[id] = Source{ID: id, Name: name, FeedURL: feed, Adapter: policy.adapter, LastAttemptedAt: attempted, LastSuccessfulAt: success, Status: status, AttributionBasis: basis}
	}
	previousObserved, previousID := maxSafeInteger, ""
	seen := map[string]bool{}
	for _, row := range referenceRows {
		item, ok := object(row, "id source_id external_id url title excerpt excerpt_available excerpt_truncated authors source_published_at source_publication_timezone_known first_observed_at last_observed_at content_hash native_identity claimable_job hugging_face_eligible untrusted_content", "")
		if !ok {
			return nil, ErrUnavailable
		}
		id, ok := text(item["id"], 64)
		if !ok || !hashPattern.MatchString(id) || seen[id] || suppressed[id] {
			return nil, ErrUnavailable
		}
		seen[id] = true
		sourceID, ok := text(item["source_id"], 64)
		if !ok {
			return nil, ErrUnavailable
		}
		source, exists := sources[sourceID]
		if !exists {
			return nil, ErrUnavailable
		}
		external, ok := text(item["external_id"], 512)
		if !ok || external == "" {
			return nil, ErrUnavailable
		}
		computed, err := ReferenceID(sourceID, external)
		if err != nil || computed != id {
			return nil, ErrUnavailable
		}
		link, ok := text(item["url"], 4096)
		if !ok || !allowedURL(link, true) {
			return nil, ErrUnavailable
		}
		if source.Adapter == cuttleAdapter && (!slugPattern.MatchString(external) || link != cuttleOrigin+"/"+external+"/") {
			return nil, ErrUnavailable
		}
		title, ok := text(item["title"], 512)
		if !ok || strings.TrimSpace(title) == "" {
			return nil, ErrUnavailable
		}
		excerpt, ok := text(item["excerpt"], 2048)
		if !ok || utf8.RuneCountInString(excerpt) > 512 {
			return nil, ErrUnavailable
		}
		available, ok := flag(item["excerpt_available"])
		if !ok {
			return nil, ErrUnavailable
		}
		truncated, ok := flag(item["excerpt_truncated"])
		if !ok || !available && (excerpt != "" || truncated) {
			return nil, ErrUnavailable
		}
		if available {
			g, ok := permitted(policies[sourceID], "full_text_storage", now)
			if !ok || until > g.expires {
				return nil, ErrUnavailable
			}
			notBefore = max(notBefore, g.reviewed)
		}
		first, ok := number(item["first_observed_at"])
		if !ok || first <= 0 {
			return nil, ErrUnavailable
		}
		last, ok := number(item["last_observed_at"])
		if !ok || first > last || last > source.LastSuccessfulAt || first > previousObserved || first == previousObserved && id <= previousID {
			return nil, ErrUnavailable
		}
		previousObserved, previousID = first, id
		contentHash, ok := text(item["content_hash"], 64)
		if !ok || !hashPattern.MatchString(contentHash) {
			return nil, ErrUnavailable
		}
		if item["native_identity"] != false || item["claimable_job"] != false || item["hugging_face_eligible"] != false || item["untrusted_content"] != true {
			return nil, ErrUnavailable
		}
		published, ok := text(item["source_published_at"], 40)
		if !ok {
			return nil, ErrUnavailable
		}
		known, ok := flag(item["source_publication_timezone_known"])
		if !ok || !publicationTime(published, known, source.Adapter) {
			return nil, ErrUnavailable
		}
		authors, ok := item["authors"].([]any)
		if !ok || len(authors) > 20 {
			return nil, ErrUnavailable
		}
		for _, rawAuthor := range authors {
			author, ok := object(rawAuthor, "name url", "")
			if !ok {
				return nil, ErrUnavailable
			}
			if _, ok = text(author["name"], 512); !ok {
				return nil, ErrUnavailable
			}
			link, ok := text(author["url"], 4096)
			if !ok || link != "" && !allowedURL(link, true) {
				return nil, ErrUnavailable
			}
		}
		if source.Adapter == cuttleAdapter {
			if len(authors) != 2 {
				return nil, ErrUnavailable
			}
			first := authors[0].(map[string]any)
			second := authors[1].(map[string]any)
			if first["name"] != "@0xCuttlefish" || first["url"] != cuttleOrigin+"/about/" || second["name"] != "Trurl (Hermes Agent; site-declared co-creator)" || second["url"] != cuttleOrigin+"/llms.txt" {
				return nil, ErrUnavailable
			}
		}
	}
	snapshot, err := decodeSnapshot(projectionRaw)
	if err != nil {
		return nil, err
	}
	snapshot.notBefore = notBefore
	return snapshot, nil
}

func publicationTime(raw string, known bool, adapter string) bool {
	if raw == "" {
		return !known && adapter != cuttleAdapter
	}
	if !known {
		if adapter != cuttleAdapter || !legacyDatePattern.MatchString(raw) {
			return false
		}
		value, err := time.Parse("2006-01-02 15:04:05", raw)
		return err == nil && value.Year() >= 1
	}
	if !datePattern.MatchString(raw) {
		return false
	}
	if !strings.HasSuffix(raw, "Z") {
		offset := raw[len(raw)-6:]
		if offset[1:3] > "23" || offset[4:6] > "59" {
			return false
		}
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && value.Year() >= 1
}
