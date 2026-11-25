//go:build !linux

package server

import (
	"context"
	"errors"
)

// Non-Linux platforms skip diskless memfd cwebp optimization and store WebP untouched.
func encodeWebP(_ context.Context, _ []byte, _ string) ([]byte, error) {
	return nil, errors.New("lossless cwebp memfd optimization requires Linux")
}
