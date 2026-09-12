package references

import (
	"context"
	"io"
	"os"
	"strings"
	"syscall"
)

func owned(info os.FileInfo, owner uint32) bool {
	value, ok := info.Sys().(*syscall.Stat_t)
	return ok && (value.Uid == 0 || value.Uid == owner)
}

func singleLink(info os.FileInfo) bool {
	value, ok := info.Sys().(*syscall.Stat_t)
	return ok && value.Nlink == 1
}

func readFixed(ctx context.Context, path string, owner uint32, limit int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	directory := os.NewFile(uintptr(fd), "/")
	defer func() { directory.Close() }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		next, err := syscall.Openat(int(directory.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, ErrUnavailable
		}
		child := os.NewFile(uintptr(next), part)
		info, err := child.Stat()
		stickyTemp := false
		if info != nil {
			attributes, ok := info.Sys().(*syscall.Stat_t)
			stickyTemp = ok && attributes.Uid == 0 && info.Mode()&os.ModeSticky != 0
		}
		if err != nil || !info.IsDir() || !owned(info, owner) || (info.Mode().Perm()&0022 != 0 && !stickyTemp) {
			child.Close()
			return nil, ErrUnavailable
		}
		directory.Close()
		directory = child
	}
	info, err := directory.Stat()
	if err != nil || !owned(info, owner) || info.Mode().Perm()&0022 != 0 {
		return nil, ErrUnavailable
	}
	fileFD, err := syscall.Openat(int(directory.Fd()), parts[len(parts)-1], syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(fileFD), "reference-input")
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || !owned(before, owner) || !singleLink(before) || before.Mode().Perm()&0022 != 0 || before.Size() > limit {
		return nil, ErrUnavailable
	}
	raw := make([]byte, 0, before.Size())
	buffer := make([]byte, 32768)
	for {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		n, readErr := file.Read(buffer)
		if int64(len(raw)+n) > limit {
			return nil, ErrUnavailable
		}
		raw = append(raw, buffer[:n]...)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, ErrUnavailable
		}
	}
	after, err := file.Stat()
	if err != nil || !owned(after, owner) || !singleLink(after) || after.Mode().Perm()&0022 != 0 || before.Size() != after.Size() || int64(len(raw)) != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, ErrUnavailable
	}
	return raw, nil
}
