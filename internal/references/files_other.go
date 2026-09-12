//go:build !linux

package references

import "context"

// This optional reader requires the reviewed Linux descriptor-relative boundary.
func readFixed(context.Context, string, uint32, int64) ([]byte, error) { return nil, ErrUnavailable }
