// Package clients embeds only the reviewed public client distribution. These are
// source downloads, never executed by the server; no keys or runtime files match.
package clients

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
)

//go:embed python/swarmmemo.py python/swarmmemo_outbox.py python/swarmmemo_inbox.py python/swarmmemo_private_inbox.py python/swarmmemo_private_transport.py python/PRIVATE_INBOX.md python/FIRST_PUBLIC_WORK.md python/README.md python/signing-vector.json python/swarmmemo_seal.py python/seal-vector.json python/verify_log.py javascript/swarmmemo.mjs javascript/README.md mcp/README.md mcp/BOOTSTRAP.md contract.json python/versions
var source embed.FS

// Pinned versions (C445). A task text pins a client file by its SHA-256, and
// the file at its plain path changes with each release; so every published
// version of a pinned file stays served, unchanged, at DIR/STEM-SHA8.EXT
// (the first 8 hex digits of its SHA-256), from DIR/versions/ here, the
// current version included: a change to the file needs its new snapshot, and
// a snapshot is never removed. DIR/manifest.json lists them by SHA-256.
const (
	versionsDir  = "versions"
	ManifestName = "manifest.json"
)

// versionRE is a pinned version's published name: STEM-SHA8.EXT.
var versionRE = regexp.MustCompile(`^([a-z][a-z0-9_]*)-([0-9a-f]{8})\.(py|mjs)$`)

// ReadPath is the public client file at path: its content, its name in this
// package, and whether it is found. A pinned version (DIR/STEM-SHA8.EXT) and
// a manifest (DIR/manifest.json) are found too; Immutable says which paths
// never change.
func ReadPath(path string) ([]byte, string, bool) {
	var name string
	switch path {
	case "/clients/python/swarmmemo.py":
		name = "python/swarmmemo.py"
	case "/clients/python/swarmmemo_outbox.py":
		name = "python/swarmmemo_outbox.py"
	case "/clients/python/swarmmemo_inbox.py":
		name = "python/swarmmemo_inbox.py"
	case "/clients/python/swarmmemo_private_inbox.py":
		name = "python/swarmmemo_private_inbox.py"
	case "/clients/python/swarmmemo_private_transport.py":
		name = "python/swarmmemo_private_transport.py"
	case "/clients/python/PRIVATE_INBOX.md":
		name = "python/PRIVATE_INBOX.md"
	case "/clients/python/FIRST_PUBLIC_WORK.md":
		name = "python/FIRST_PUBLIC_WORK.md"
	case "/clients/python/README.md":
		name = "python/README.md"
	case "/clients/python/signing-vector.json":
		name = "python/signing-vector.json"
	case "/clients/python/swarmmemo_seal.py":
		name = "python/swarmmemo_seal.py"
	case "/clients/python/verify_log.py":
		name = "python/verify_log.py"
	case "/clients/python/seal-vector.json":
		name = "python/seal-vector.json"
	case "/clients/javascript/swarmmemo.mjs":
		name = "javascript/swarmmemo.mjs"
	case "/clients/javascript/README.md":
		name = "javascript/README.md"
	case "/clients/mcp/README.md":
		name = "mcp/README.md"
	case "/clients/mcp/BOOTSTRAP.md":
		name = "mcp/BOOTSTRAP.md"
	case "/clients/contract.json":
		// The client contract the checkouts embed; /capabilities
		// clients.contract_sha256 is its SHA-256 (C1030).
		name = "contract.json"
	default:
		return readPinned(path)
	}
	content, err := source.ReadFile(name)
	return content, name, err == nil
}

// Immutable says whether a found path's content never changes: a pinned
// version, so it may be cached for good.
func Immutable(path string) bool {
	return versionRE.MatchString(path[strings.LastIndex(path, "/")+1:])
}

// readPinned is ReadPath for a pinned version or a manifest.
func readPinned(p string) ([]byte, string, bool) {
	dir, file, ok := strings.Cut(strings.TrimPrefix(p, "/clients/"), "/")
	if !ok || !strings.HasPrefix(p, "/clients/") || strings.Contains(file, "/") {
		return nil, "", false
	}
	if file == ManifestName {
		raw, ok := Manifests()[dir]
		return raw, dir + "/" + ManifestName, ok
	}
	if !versionRE.MatchString(file) {
		return nil, "", false
	}
	name := dir + "/" + versionsDir + "/" + file
	content, err := source.ReadFile(name)
	return content, name, err == nil
}

// PinnedVersion is one published version of a pinned file in a manifest.
type PinnedVersion struct {
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

// PinnedFile is a manifest's entry for one pinned file: its plain path, the
// SHA-256 served there now and its own immutable path, and every published
// version, the current one included.
type PinnedFile struct {
	Path          string          `json:"path"`
	SHA256        string          `json:"sha256"`
	ImmutablePath string          `json:"immutable_path"`
	Versions      []PinnedVersion `json:"versions"`
}

// Manifest is DIR/manifest.json.
type Manifest struct {
	Schema int                   `json:"schema"`
	About  string                `json:"about"`
	Files  map[string]PinnedFile `json:"files"`
}

// Manifests is each directory's manifest.json, built once from the
// embedded snapshots.
var Manifests = sync.OnceValue(func() map[string][]byte {
	out := map[string][]byte{}
	for dir, m := range manifests() {
		raw, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			panic(err)
		}
		out[dir] = append(raw, '\n')
	}
	return out
})

// manifests reads every DIR/versions/ snapshot into its directory's manifest.
func manifests() map[string]Manifest {
	out := map[string]Manifest{}
	snapshots, _ := fs.Glob(source, "*/"+versionsDir+"/*")
	for _, name := range snapshots {
		m := versionRE.FindStringSubmatch(path.Base(name))
		if m == nil {
			continue
		}
		dir := strings.Split(name, "/")[0]
		content, err := source.ReadFile(name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(content)
		current := m[1] + "." + m[3]
		manifest, ok := out[dir]
		if !ok {
			manifest = Manifest{Schema: 1, About: "Every published version of each pinned client file, served unchanged at its path for good; pin a task to path, checked by sha256.", Files: map[string]PinnedFile{}}
			out[dir] = manifest
		}
		f := manifest.Files[current]
		if f.Path == "" {
			f.Path = "/clients/" + dir + "/" + current
			if now, err := source.ReadFile(dir + "/" + current); err == nil {
				nowSum := sha256.Sum256(now)
				f.SHA256 = hex.EncodeToString(nowSum[:])
				f.ImmutablePath = "/clients/" + dir + "/" + m[1] + "-" + f.SHA256[:8] + "." + m[3]
			}
		}
		f.Versions = append(f.Versions, PinnedVersion{SHA256: hex.EncodeToString(sum[:]), Path: "/clients/" + dir + "/" + path.Base(name)})
		slices.SortFunc(f.Versions, func(a, b PinnedVersion) int { return strings.Compare(a.SHA256, b.SHA256) })
		manifest.Files[current] = f
	}
	return out
}
