package imageutil

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func orientedJPEG(t *testing.T, orientation byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	colors := []color.RGBA{{240, 10, 10, 255}, {10, 240, 10, 255}, {10, 10, 240, 255}, {240, 240, 10, 255}}
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			index := 0
			if x >= 30 {
				index++
			}
			if y >= 20 {
				index += 2
			}
			img.Set(x, y, colors[index])
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	// TIFF little-endian IFD0: a single SHORT Orientation tag.
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, orientation, 0, 0, 0, 0, 0, 0, 0}
	var raw bytes.Buffer
	raw.Write([]byte{0xff, 0xd8, 0xff, 0xe1})
	binary.Write(&raw, binary.BigEndian, uint16(len(exif)+2))
	raw.Write(exif)
	raw.Write(encoded.Bytes()[2:])
	return raw.Bytes()
}

func TestThumbnailAppliesAllEXIFOrientationsWithoutChangingSource(t *testing.T) {
	quadrants := [][4]int{{0, 1, 2, 3}, {1, 0, 3, 2}, {3, 2, 1, 0}, {2, 3, 0, 1}, {0, 2, 1, 3}, {2, 0, 3, 1}, {3, 1, 2, 0}, {1, 3, 0, 2}}
	colors := []color.RGBA{{240, 10, 10, 255}, {10, 240, 10, 255}, {10, 10, 240, 255}, {240, 240, 10, 255}}
	for orientation := byte(1); orientation <= 8; orientation++ {
		raw := orientedJPEG(t, orientation)
		before := bytes.Clone(raw)
		out, mime, err := GenerateThumbnail(raw, 320)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		w, h := 60, 40
		if orientation >= 5 {
			w, h = h, w
		}
		if mime != "image/jpeg" || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
			t.Fatalf("orientation %d dimensions: %s %v", orientation, mime, img.Bounds())
		}
		points := [][2]int{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}}
		for i, p := range points {
			got := color.RGBAModel.Convert(img.At(p[0], p[1])).(color.RGBA)
			want := colors[quadrants[orientation-1][i]]
			if abs(int(got.R)-int(want.R))+abs(int(got.G)-int(want.G))+abs(int(got.B)-int(want.B)) > 60 {
				t.Fatalf("orientation %d quadrant %d got %v want %v", orientation, i, got, want)
			}
		}
		if !bytes.Equal(raw, before) {
			t.Fatal("original JPEG mutated")
		}
		out, _, err = GenerateThumbnail(raw, 15)
		if err != nil {
			t.Fatal(err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if orientation >= 5 && (cfg.Width != 10 || cfg.Height != 15) {
			t.Fatalf("oriented resize: %dx%d", cfg.Width, cfg.Height)
		}
	}
}
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
