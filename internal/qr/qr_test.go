package qr

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeQRPNG draws a code for the external decoder check (ZL_QR_OUT=dir go test -run TestQR).
func writeQRPNG(q *Code, p string) error {
	const scale, quiet = 8, 4
	n := (q.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			mx, my := x/scale-quiet, y/scale-quiet
			c := color.Gray{255}
			if mx >= 0 && my >= 0 && mx < q.Size && my < q.Size && q.Dark(mx, my) {
				c = color.Gray{0}
			}
			img.SetGray(x, y, c)
		}
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

var qrSamples = []string{
	"hi",
	"http://192.168.1.23:17891/m#k=7KQ2M9XR4TVB8HJD",
	"http://10.0.0.5:17891/m#k=ABCDEFGHJKMNPQRS",
	strings.Repeat("Zipline ", 12),
	strings.Repeat("0123456789abcdef", 8),
	strings.Repeat("Q", 150),
	strings.Repeat("x", 213),
}

func TestQR(t *testing.T) {
	out := os.Getenv("ZL_QR_OUT")
	for i, s := range qrSamples {
		q, err := Encode(s)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if q.Size < 21 || q.Size > 57 || !strings.HasPrefix(q.SVG(), "<svg") {
			t.Fatalf("%d: size %d", i, q.Size)
		}
		if out != "" {
			if err := writeQRPNG(q, filepath.Join(out, fmt.Sprintf("qr%d.png", i))); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(out, fmt.Sprintf("qr%d.txt", i)), []byte(s), 0o644)
		}
	}
	if _, err := Encode(strings.Repeat("x", 214)); err == nil {
		t.Fatal("214 bytes don't fit")
	}
	// Reed-Solomon: the example from the QR standard's annex (version 1-M, "01234567")
	data := []byte{0x10, 0x20, 0x0C, 0x56, 0x61, 0x80, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11}
	want := []byte{0xA5, 0x24, 0xD4, 0xC1, 0xED, 0x36, 0xC7, 0x87, 0x2C, 0x55}
	if got := rsRemainder(data, 10); string(got) != string(want) {
		t.Fatalf("RS %x", got)
	}
}
