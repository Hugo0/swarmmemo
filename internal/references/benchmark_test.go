package references

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in and fixed small iteration counts prevent an accidental long
// calibration run. This measures authorization, not rendering or an HTTP SLO.
// SWARMMEMO_REFERENCE_BENCH=1 go test ./internal/references -run '^$'
// -bench '^BenchmarkProtectedReferenceRead$' -benchtime=3x -count=1 -benchmem
func BenchmarkProtectedReferenceRead(b *testing.B) {
	if os.Getenv("SWARMMEMO_REFERENCE_BENCH") != "1" {
		b.Skip("set SWARMMEMO_REFERENCE_BENCH=1 and -benchtime=3x (maximum 10x)")
	}
	for _, size := range []struct {
		name  string
		count int
		large bool
	}{{"24", 24, false}, {"1000", 1000, false}, {"1000Near8MiB", 1000, true}} {
		count := size.count
		for _, operation := range []string{"Load", "LoadAndFence"} {
			if size.large && operation == "Load" {
				continue
			}
			b.Run(size.name+"/"+operation, func(b *testing.B) {
				if b.N > 10 {
					b.Fatal("use explicit -benchtime=3x through 10x; no automatic calibration")
				}
				registry, suppression, snapshot := fixture(b)
				base := snapshot.References[0]
				// Exactly 512 Unicode scalars, representative UTF-8 prose, two
				// realistically sized author names and HTTPS attribution URLs.
				base.Excerpt = strings.Repeat("A short source excerpt. ", 21) + "雪の記録 abc"
				if len([]rune(base.Excerpt)) != 512 {
					b.Fatal("excerpt fixture changed")
				}
				base.Authors = []Author{{Name: "External human author", URL: "https://example.com/about/"}, {Name: "Disclosed agent co-creator", URL: "https://example.com/agents/research-assistant/"}}
				if size.large {
					for index := range base.Authors {
						prefix := "https://example.com/author/"
						base.Authors[index].URL = prefix + strings.Repeat("a", 3500-len(prefix))
					}
				}
				snapshot.References = make([]Reference, count)
				for index := range snapshot.References {
					item := base
					item.ExternalID = fmt.Sprintf("article-%04d", index)
					item.ID, _ = ReferenceID("fixture", item.ExternalID)
					item.URL = "https://example.com/articles/" + item.ExternalID + "/"
					item.Title = fmt.Sprintf("External research progress note %04d", index)
					snapshot.References[index] = item
				}
				sort.Slice(snapshot.References, func(i, j int) bool { return snapshot.References[i].ID < snapshot.References[j].ID })
				projection := mustCanonical(b, snapshot)
				if len(projection) > 8<<20 || size.large && len(projection) < 7<<20 {
					b.Fatal("projection fixture outside expected byte bound")
				}
				directory := b.TempDir()
				if err := os.Chmod(directory, 0700); err != nil {
					b.Fatal(err)
				}
				config := Config{RegistryPath: filepath.Join(directory, "registry.json"), SuppressionPath: filepath.Join(directory, "suppression.json"), SnapshotPath: filepath.Join(directory, "projection.json"), OwnerUID: uint32(os.Getuid())}
				for path, raw := range map[string][]byte{config.RegistryPath: registry, config.SuppressionPath: suppression, config.SnapshotPath: projection} {
					if err := os.WriteFile(path, raw, 0640); err != nil {
						b.Fatal(err)
					}
				}
				reader, err := New(config)
				if err != nil {
					b.Fatal(err)
				}
				reader.now = func() time.Time { return time.Unix(fixtureNow, 0) }
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					loaded, err := reader.Load(ctx)
					if err != nil || len(loaded.References) != count {
						b.Fatal("Load failed", err)
					}
					if operation == "LoadAndFence" {
						if err := reader.Fence(ctx, loaded); err != nil {
							b.Fatal("Fence failed", err)
						}
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(len(projection)), "projection_bytes")
				b.ReportMetric(float64(len(registry)), "registry_bytes")
				b.ReportMetric(float64(len(suppression)), "suppression_bytes")
			})
		}
	}
}
