package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"github.com/3922510-creator/pic-tg/storage"
	"testing"
	"time"
)

func processFixture(t *testing.T, data []byte, format string) ([]byte, int, int, string) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, w, h, reason, err := prepareUpload(context.Background(), data, format, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > len(data) {
		t.Fatal("larger than original")
	}
	return out, w, h, reason
}

func TestJPEGLosslessAndFallback(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2800, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 2800; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x*17 + y*3), uint8(x*3 + y*21), uint8(x*11 + y*7), 255})
		}
	}
	var src bytes.Buffer
	jpeg.Encode(&src, img, &jpeg.Options{Quality: 98})
	out, w, h, _ := processFixture(t, src.Bytes(), "jpeg")
	if w != 2800 || h != 64 {
		t.Fatal("JPEG dimensions changed")
	}
	assertSamePixels(t, src.Bytes(), out)
	var tiny bytes.Buffer
	jpeg.Encode(&tiny, image.NewGray(image.Rect(0, 0, 4, 3)), &jpeg.Options{Quality: 1})
	original := tiny.Bytes()
	out, _, _, _ = processFixture(t, original, "jpeg")
	if len(out) > len(original) {
		t.Fatal("larger than original")
	}
	assertSamePixels(t, original, out)
}

func TestJPEGOrientationPreserved(t *testing.T) {
	var src bytes.Buffer
	jpeg.Encode(&src, image.NewNRGBA(image.Rect(0, 0, 100, 50)), &jpeg.Options{Quality: 100})
	// EXIF little-endian orientation=6 (90 degrees clockwise).
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	segment := []byte{255, 225, 0, byte(len(exif) + 2)}
	data := append([]byte{255, 216}, segment...)
	data = append(data, exif...)
	data = append(data, src.Bytes()[2:]...)
	// Force the successful re-encode branch without making the decoder work harder.
	data = append(data, make([]byte, 4096)...)
	out, w, h, _ := processFixture(t, data, "jpeg")
	if w != 100 || h != 50 || !bytes.Contains(out, []byte("Exif\x00\x00")) {
		t.Fatal("EXIF orientation or original dimensions changed")
	}
}

func TestPNGLosslessTransparency(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 300; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 100, uint8(x + y)})
		}
	}
	var src bytes.Buffer
	(&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&src, img)
	out, w, h, _ := processFixture(t, src.Bytes(), "png")
	if len(out) >= src.Len() || w != 300 || h != 120 {
		t.Fatal("PNG not optimized")
	}
	decoded, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 120; y++ {
		for x := 0; x < 300; x++ {
			if color.NRGBAModel.Convert(decoded.At(x, y)) != color.NRGBAModel.Convert(img.At(x, y)) {
				t.Fatal("PNG pixels or alpha changed")
			}
		}
	}
}

func pngChunk(kind string, payload []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(payload)))
	b.WriteString(kind)
	b.Write(payload)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), payload...)))
	return b.Bytes()
}

func TestAnimationPreserved(t *testing.T) {
	raw := pngFixture(t)
	data := append([]byte(nil), raw[:33]...)
	data = append(data, pngChunk("acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0})...)
	data = append(data, raw[33:]...)
	out, _, _, reason := processFixture(t, data, "png")
	if !bytes.Equal(out, data) || reason != "保留动态图" {
		t.Fatal("APNG flattened")
	}
	for _, kind := range []string{"ANIM", "ANMF"} {
		webp := append([]byte("RIFF\x0c\x00\x00\x00WEBP"), []byte(kind)...)
		webp = append(webp, 0, 0, 0, 0)
		if !hasAnimation(webp, "webp") {
			t.Fatal("WebP animation not recognized")
		}
	}
	if !hasAnimation(nil, "gif") {
		t.Fatal("GIF not skipped")
	}
}

func TestWebPCompressionAndCancellation(t *testing.T) {
	// Use the actual installed encoder, not a substitute implementation.
	img := image.NewNRGBA(image.Rect(0, 0, 256, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 256; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 3), uint8(y * 7), uint8(x + y), 128})
		}
	}
	var src bytes.Buffer
	png.Encode(&src, img)
	data, err := encodeWebP(context.Background(), src.Bytes(), "100")
	if err != nil {
		t.Fatal(err)
	}
	out, w, h, _ := processFixture(t, data, "webp")
	assertSamePixels(t, data, out)
	if w != 256 || h != 128 {
		t.Fatal("WebP resized")
	}
	decoded, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a := decoded.At(10, 10).RGBA()
	if a != 128*257 {
		t.Fatal("WebP transparency lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
	if _, _, _, _, err := prepareUpload(ctx, data, "webp", cfg); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestUploadUsesServerCompression(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2800, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 2800; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 7), uint8(y * 3), uint8(x + y), 180})
		}
	}
	var jpg, pngData bytes.Buffer
	jpeg.Encode(&jpg, img, &jpeg.Options{Quality: 98})
	(&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&pngData, img)
	webp, err := encodeWebP(context.Background(), pngData.Bytes(), "100")
	if err != nil {
		t.Fatal(err)
	}
	for format, data := range map[string][]byte{"jpeg": jpg.Bytes(), "png": pngData.Bytes(), "webp": webp} {
		t.Run(format, func(t *testing.T) {
			srv, cleanup := setupTestServer(t)
			defer cleanup()
			out, w, h, _ := processFixture(t, data, format)
			sum := sha256.Sum256(out)
			key := fmt.Sprintf("telegram/0123456789abcdef/%s/%x.%s", time.Now().UTC().Format("2006/01/02"), sum[:6], format)
			if err := srv.db.SaveObject(&storage.ObjectRecord{Key: key, ConnectionID: "0123456789abcdef", FID: "fixture", FileID: "fixture", Size: int64(len(out)), OrigSize: int64(len(data)), Width: w, Height: h, Format: format}); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, _ := writer.CreateFormFile("file", "original."+format)
			part.Write(data)
			writer.WriteField("link_mode", "direct") // Legacy clients must also receive a proxy URL.
			writer.Close()
			req := httptest.NewRequest("POST", "/api/upload", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Authorization", "Bearer test-pass-123")
			recorder := httptest.NewRecorder()
			srv.Handler().ServeHTTP(recorder, req)
			if recorder.Code != 200 {
				t.Fatal(recorder.Code, recorder.Body.String())
			}
			var response struct {
				Data struct {
					Key      string `json:"key"`
					URL      string `json:"url"`
					Size     int64  `json:"size"`
					OrigSize int64  `json:"orig_size"`
					Width    int    `json:"width"`
					Height   int    `json:"height"`
				}
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			expectedURL, err := srv.publicURL(key)
			if err != nil {
				t.Fatal(err)
			}
			if response.Data.URL != expectedURL {
				t.Fatalf("legacy link_mode bypassed proxy: %s", response.Data.URL)
			}
			if response.Data.Key != key || response.Data.Size != int64(len(out)) || response.Data.OrigSize != int64(len(data)) || response.Data.Width != w || response.Data.Height != h {
				t.Fatalf("processing path skipped: %+v", response)
			}
		})
	}
}

func assertSamePixels(t *testing.T, a, b []byte) {
	t.Helper()
	x, _, e := image.Decode(bytes.NewReader(a))
	if e != nil {
		t.Fatal(e)
	}
	y, _, e := image.Decode(bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	if x.Bounds() != y.Bounds() {
		t.Fatal("bounds changed")
	}
	for j := x.Bounds().Min.Y; j < x.Bounds().Max.Y; j++ {
		for i := x.Bounds().Min.X; i < x.Bounds().Max.X; i++ {
			if color.NRGBA64Model.Convert(x.At(i, j)) != color.NRGBA64Model.Convert(y.At(i, j)) {
				t.Fatalf("pixel changed at %d,%d", i, j)
			}
		}
	}
}

func TestPNGMetadataChunksRetained(t *testing.T) {
	raw := pngFixture(t)
	metadata := pngChunk("tEXt", []byte("Comment\x00keep this metadata"))
	data := append([]byte(nil), raw[:33]...)
	data = append(data, metadata...)
	data = append(data, raw[33:]...)
	data = append(data, make([]byte, 2048)...)
	out, _, _, _ := processFixture(t, data, "png")
	if !bytes.Contains(out, metadata) {
		t.Fatal("PNG ancillary metadata removed")
	}
	assertSamePixels(t, data, out)
}
