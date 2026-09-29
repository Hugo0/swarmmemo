package cards

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The cache is one file per card in its own directory: a JSON header line,
// then the PNG. A card has exactly one file, named by its kind and a hash of
// its key, so a re-render replaces it and a removal deletes it. The directory
// holds only rendered public content and can be deleted at any time.
//
// A cached image is never served on its own authority: the image route first
// reads the card from the board, refusing anything not public and visible, and
// only then compares digests. Invalidation on edit or hide is that comparison.

const cacheExt = ".card"

type entry struct {
	Digest   string   `json:"digest"`
	Parts    []string `json:"parts,omitempty"`
	Renderer string   `json:"renderer"`
	Created  int64    `json:"created"`
	PNG      []byte   `json:"-"`
}

type diskCache struct {
	dir   string
	max   int64
	mu    sync.Mutex
	total int64
	sizes map[string]int64
}

func openCache(dir string, max int64) (*diskCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &diskCache{dir: dir, max: max, sizes: map[string]int64{}}
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		name := item.Name()
		if strings.HasPrefix(name, ".tmp-") {
			// A write interrupted by a crash; never renamed into place.
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, cacheExt) {
			continue
		}
		if info, err := item.Info(); err == nil {
			c.sizes[name] = info.Size()
			c.total += info.Size()
		}
	}
	return c, nil
}

func cacheName(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return kind + "-" + hex.EncodeToString(sum[:16]) + cacheExt
}

func (c *diskCache) get(kind, key string) (entry, bool) {
	path := filepath.Join(c.dir, cacheName(kind, key))
	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxImageBytes+64<<10 {
		return entry{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return entry{}, false
	}
	head, png, found := bytes.Cut(raw, []byte("\n"))
	var e entry
	if !found || json.Unmarshal(head, &e) != nil || checkPNG(png) != nil {
		c.forget(kind, key)
		return entry{}, false
	}
	e.PNG = png
	// Reads refresh the time eviction goes by, so eviction is least recently used.
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return e, true
}

func (c *diskCache) put(kind, key string, e entry) error {
	if len(e.PNG) > MaxImageBytes {
		return errors.New("cards: image exceeds the size cap")
	}
	head, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(append(head, '\n'), e.PNG...))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	name := cacheName(kind, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Rename(tmp.Name(), filepath.Join(c.dir, name)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	size := int64(len(head) + 1 + len(e.PNG))
	c.total += size - c.sizes[name]
	c.sizes[name] = size
	c.evictLocked(name)
	return nil
}

func (c *diskCache) forget(kind, key string) {
	name := cacheName(kind, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Remove(filepath.Join(c.dir, name)); err == nil || errors.Is(err, fs.ErrNotExist) {
		c.total -= c.sizes[name]
		delete(c.sizes, name)
	}
}

// evictLocked removes least recently used files until the directory is back
// under 90% of its cap, keeping the file just written.
func (c *diskCache) evictLocked(keep string) {
	if c.total <= c.max {
		return
	}
	type aged struct {
		name string
		at   time.Time
	}
	var files []aged
	for name := range c.sizes {
		if name == keep {
			continue
		}
		if info, err := os.Stat(filepath.Join(c.dir, name)); err == nil {
			files = append(files, aged{name, info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files {
		if c.total <= c.max*9/10 {
			return
		}
		if err := os.Remove(filepath.Join(c.dir, f.name)); err == nil || errors.Is(err, fs.ErrNotExist) {
			c.total -= c.sizes[f.name]
			delete(c.sizes, f.name)
		}
	}
}

func (c *diskCache) size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
