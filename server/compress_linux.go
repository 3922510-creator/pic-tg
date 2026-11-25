//go:build linux

package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// cwebp needs seekable input, so use an anonymous kernel-memory fd on Linux
// instead of writing a temporary file to disk.
func encodeWebP(ctx context.Context, data []byte, quality string) ([]byte, error) {
	_, cwebp := lookupImageTools()
	if cwebp == "" {
		return nil, errors.New("cwebp not installed")
	}
	fd, err := unix.MemfdCreate("pic-tg-webp", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	input := os.NewFile(uintptr(fd), "pic-tg-webp")
	defer input.Close()
	if _, err := input.Write(data); err != nil {
		return nil, err
	}
	if _, err := input.Seek(0, 0); err != nil {
		return nil, err
	}
	child, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	args := []string{"-quiet", "-q", quality, "-alpha_q", "100", "-metadata", "all"}
	if quality == "lossless" {
		args = []string{"-quiet", "-lossless", "-exact", "-q", "75", "-metadata", "all"}
	}
	args = append(args, "/proc/self/fd/3", "-o", "/dev/stdout")
	cmd := exec.CommandContext(child, cwebp, args...)
	cmd.ExtraFiles = []*os.File{input}
	return cmd.Output()
}
