package references

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProtectedReferenceFiles(t *testing.T) {
	for _, mode := range []string{"safe read group", "world writable", "group writable", "file symlink", "hardlink", "parent symlink", "writable parent", "writable ancestor", "FIFO", "directory", "wrong owner", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			r, config, _ := fileFixture(t)
			switch mode {
			case "world writable":
				os.Chmod(config.RegistryPath, 0666)
			case "group writable":
				os.Chmod(config.SuppressionPath, 0660)
			case "file symlink":
				real := config.RegistryPath + ".real"
				os.Rename(config.RegistryPath, real)
				os.Symlink(real, config.RegistryPath)
			case "parent symlink":
				link := filepath.Join(t.TempDir(), "linked")
				os.Symlink(filepath.Dir(config.RegistryPath), link)
				r.config.RegistryPath = filepath.Join(link, "registry.json")
			case "hardlink":
				if err := os.Link(config.RegistryPath, config.RegistryPath+".alias"); err != nil {
					t.Fatal(err)
				}
			case "writable parent":
				os.Chmod(filepath.Dir(config.RegistryPath), 0777)
			case "writable ancestor":
				ancestor := filepath.Dir(filepath.Dir(config.RegistryPath))
				if err := os.Chmod(ancestor, 0777); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(ancestor, 0700)
			case "FIFO":
				os.Remove(config.RegistryPath)
				if err := syscall.Mkfifo(config.RegistryPath, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				os.Remove(config.RegistryPath)
				os.Mkdir(config.RegistryPath, 0700)
			case "wrong owner":
				if os.Getuid() == 0 {
					t.Skip("root files are explicitly permitted")
				}
				r.config.OwnerUID = uint32(os.Getuid() + 1)
			case "oversize":
				os.WriteFile(config.RegistryPath, []byte(strings.Repeat(" ", (1<<20)+1)), 0640)
			}
			start := time.Now()
			_, err := r.Load(context.Background())
			if mode == "safe read group" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatal("accepted unsafe file", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("nonregular file blocked reader")
			}
		})
	}
}
