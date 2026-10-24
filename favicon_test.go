package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestFavicon(t *testing.T) {
	w := request(NewLiveServer(testStore(t)), "GET", "/favicon.ico")
	if w.Code != 200 {
		t.Fatalf("favicon status: %d", w.Code)
	}
	for name, want := range map[string]string{
		"Content-Type":            "image/x-icon",
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	icon, err := frontend.ReadFile("web/favicon.ico")
	if err != nil || !bytes.Equal(w.Body.Bytes(), icon) {
		t.Fatalf("response differs from embedded icon: %v", err)
	}
	if len(icon) < 54 || !bytes.Equal(icon[:6], []byte{0, 0, 1, 0, 3, 0}) {
		t.Fatal("expected ICO header with three images")
	}
	offset := 54
	for i, size := range []int{16, 32, 48} {
		entry := icon[6+i*16 : 22+i*16]
		length := int(binary.LittleEndian.Uint32(entry[8:12]))
		start := int(binary.LittleEndian.Uint32(entry[12:16]))
		if int(entry[0]) != size || int(entry[1]) != size || entry[2] != 0 || entry[3] != 0 || binary.LittleEndian.Uint16(entry[4:6]) != 1 || binary.LittleEndian.Uint16(entry[6:8]) != 32 || start != offset || length <= 0 || start+length > len(icon) {
			t.Fatalf("invalid %dx%d ICO directory entry", size, size)
		}
		img, err := png.Decode(bytes.NewReader(icon[start : start+length]))
		if err != nil {
			t.Fatalf("%dx%d PNG: %v", size, size, err)
		}
		if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("unexpected PNG dimensions: %v", img.Bounds())
		}
		// The slash is white at its center, with an opaque black square behind it.
		for _, point := range [][3]int{{0, 0, 0}, {size/2 - 1, size / 2, 65535}, {size / 4, size / 2, 0}} {
			r, g, b, a := img.At(point[0], point[1]).RGBA()
			if r != uint32(point[2]) || g != r || b != r || a != 65535 {
				t.Fatalf("%dx%d pixel %v: %d %d %d %d", size, size, point[:2], r, g, b, a)
			}
		}
		offset += length
	}
	if offset != len(icon) {
		t.Fatal("unexpected trailing ICO data")
	}
}
