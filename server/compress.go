package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/3922510-creator/pic-tg/imageutil"
)

var (
	toolsOnce    sync.Once
	jpegtranPath string
	cwebpPath    string
)

func findBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"} {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate
		}
	}
	return ""
}

func lookupImageTools() (jpegtran, cwebp string) {
	toolsOnce.Do(func() {
		jpegtranPath = findBinary("jpegtran")
		cwebpPath = findBinary("cwebp")
		if jpegtranPath == "" || cwebpPath == "" {
			log.Printf("optional lossless optimizers: jpegtran=%v cwebp=%v (missing tools fall back to storing originals untouched)", jpegtranPath != "", cwebpPath != "")
		}
	})
	return jpegtranPath, cwebpPath
}

// hasAnimation walks container chunks rather than matching compressed pixel bytes.
// Malformed containers are left untouched as well; never flatten an uncertain animation.
func hasAnimation(data []byte, format string) bool {
	if format == "gif" {
		return true
	}
	pos, little := 0, false
	switch format {
	case "png":
		pos = 8
	case "webp":
		pos, little = 12, true
	default:
		return false
	}
	for pos+8 <= len(data) {
		n := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		kind := string(data[pos+4 : pos+8])
		extra := uint64(12)
		if little {
			n = uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
			kind = string(data[pos : pos+4])
			extra = 8 + n%2
		}
		if n+extra > uint64(len(data)-pos) {
			return true
		}
		if kind == "acTL" || kind == "ANIM" || kind == "ANMF" {
			return true
		}
		pos += int(n + extra)
	}
	return false
}

// prepareUpload performs only lossless coding optimization. Metadata (including
// orientation and color profiles), dimensions and animation are retained.
func prepareUpload(ctx context.Context, data []byte, format string, cfg image.Config) ([]byte, int, int, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, "", err
	}
	w, h := cfg.Width, cfg.Height
	if hasAnimation(data, format) {
		return data, w, h, "保留动态图", nil
	}
	var out []byte
	var err error
	switch format {
	case "jpeg":
		jpegtran, _ := lookupImageTools()
		if jpegtran == "" {
			return data, w, h, "保留原图（未安装 jpegtran）", nil
		}
		child, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(child, jpegtran, "-copy", "all", "-optimize")
		cmd.Stdin = bytes.NewReader(data)
		out, err = cmd.Output()
	case "png":
		out, err = optimizePNG(ctx, data)
	case "webp":
		out, err = encodeWebP(ctx, data, "lossless")
	default:
		return data, w, h, "保留原格式", nil
	}
	if ctx.Err() != nil {
		return nil, 0, 0, "", ctx.Err()
	}
	// A malformed input/optimizer failure must never cause a lossy fallback.
	if err != nil || len(out) == 0 || len(out) >= len(data) {
		return data, w, h, "保留原图（无损优化未减小）", nil
	}
	check, got, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || got != format || check.Width != w || check.Height != h || int64(w)*int64(h) > imageutil.MaxImagePixels {
		return nil, 0, 0, "", fmt.Errorf("无损优化结果校验失败")
	}
	return out, w, h, "无损优化", nil
}

// Recompress the existing PNG scanline stream without decoding/rebuilding
// pixels or touching ancillary chunks. This retains profiles and hidden RGB.
func optimizePNG(ctx context.Context, data []byte) ([]byte, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("invalid PNG header")
	}
	var packed bytes.Buffer
	pos := 8
	for pos+12 <= len(data) {
		n := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if n > uint64(len(data)-pos-12) {
			return nil, fmt.Errorf("invalid PNG chunk")
		}
		chunkLen := int(n)
		if string(data[pos+4:pos+8]) == "IDAT" {
			packed.Write(data[pos+8 : pos+8+chunkLen])
		}
		pos += chunkLen + 12
	}
	reader, err := zlib.NewReader(&packed)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var compressed bytes.Buffer
	writer, err := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if err != nil {
		return nil, err
	}
	// Filter bytes plus scanlines are bounded by the pixel budget. Stream rather
	// than materialize up to 400MB of 16-bit RGBA scanline data.
	count, err := io.Copy(writer, io.LimitReader(reader, 512*1024*1024+1))
	if count > 512*1024*1024 {
		return nil, fmt.Errorf("PNG scanlines exceed limit")
	}
	if err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(data[:8])
	written := false
	pos = 8
	for pos+12 <= len(data) {
		n := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if n > uint64(len(data)-pos-12) {
			return nil, fmt.Errorf("invalid PNG chunk")
		}
		chunkLen := int(n)
		if string(data[pos+4:pos+8]) == "IDAT" {
			if !written {
				payload := compressed.Bytes()
				binary.Write(&out, binary.BigEndian, uint32(len(payload)))
				out.WriteString("IDAT")
				out.Write(payload)
				crc := crc32.NewIEEE()
				crc.Write([]byte("IDAT"))
				crc.Write(payload)
				binary.Write(&out, binary.BigEndian, crc.Sum32())
				written = true
			}
		} else {
			out.Write(data[pos : pos+chunkLen+12])
		}
		pos += chunkLen + 12
	}
	if !written {
		return nil, fmt.Errorf("missing PNG IDAT chunk")
	}
	out.Write(data[pos:])
	return out.Bytes(), nil
}
