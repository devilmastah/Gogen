package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// gradientImage builds a synthetic image with per-pixel variation, so a
// downscale actually changes the bytes (a flat image compresses to nothing).
func gradientImage(w, h int, alpha bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha {
				a = uint8(128 + (x+y)%128)
			}
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x * y) % 256),
				A: a,
			})
		}
	}
	return img
}

func pngDataURL(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func jpegDataURL(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestNormalizeImageShrinksOversizedImage is the traffic property: an image
// larger than the model's working resolution is downscaled to it, and the
// attached payload shrinks by a large factor (it is re-uploaded every round).
func TestNormalizeImageShrinksOversizedImage(t *testing.T) {
	src := pngDataURL(t, gradientImage(3200, 2000, false))
	got := normalizeImage(llm.ImageInput{DataURL: src, Detail: "auto"})

	if got.DataURL == src {
		t.Fatal("oversized image was not touched")
	}
	_, data, ok := splitImageDataURL(got.DataURL)
	if !ok {
		t.Fatalf("result is not a base64 image data URL: %q", got.DataURL[:min(40, len(got.DataURL))])
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if max(cfg.Width, cfg.Height) > imageMaxEdgeDefaultPx {
		t.Fatalf("result long edge = %d, want <= %d", max(cfg.Width, cfg.Height), imageMaxEdgeDefaultPx)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		t.Fatal("result has no dimensions")
	}
	before := dataURLRawBytes(src)
	after := dataURLRawBytes(got.DataURL)
	if after >= before {
		t.Fatalf("payload grew: %d -> %d", before, after)
	}
	if got.Detail != "auto" {
		t.Fatalf("Detail = %q, want the caller's detail preserved", got.Detail)
	}
}

// TestNormalizeImageShrinksOversizedJPEG covers the JPEG input path: an
// already-JPEG attachment above the pixel budget is downscaled too (a photo
// straight from a camera roll is the common case there).
func TestNormalizeImageShrinksOversizedJPEG(t *testing.T) {
	src := jpegDataURL(t, noiseImage(2400, 1600))
	got := normalizeImage(llm.ImageInput{DataURL: src, Detail: "auto"})
	if got.DataURL == src {
		t.Fatal("oversized JPEG was not touched")
	}
	_, data, ok := splitImageDataURL(got.DataURL)
	if !ok {
		t.Fatal("result is not a data URL")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if max(cfg.Width, cfg.Height) > imageMaxEdgeDefaultPx {
		t.Fatalf("result long edge = %d, want <= %d", max(cfg.Width, cfg.Height), imageMaxEdgeDefaultPx)
	}
	if dataURLRawBytes(got.DataURL) >= dataURLRawBytes(src) {
		t.Fatalf("payload grew: %d -> %d", dataURLRawBytes(src), dataURLRawBytes(got.DataURL))
	}
}

// noiseImage builds a photographic-style image (incompressible pixels), the
// shape of a real screenshot or photo rather than a gradient.
func noiseImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(987654321)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255})
		}
	}
	return img
}

// TestNormalizeImageShrinksRealisticScreenshot is the byte claim on an
// incompressible image: the payload that is re-uploaded every round shrinks by
// roughly an order of magnitude.
func TestNormalizeImageShrinksRealisticScreenshot(t *testing.T) {
	src := pngDataURL(t, noiseImage(1800, 1800))
	before := dataURLRawBytes(src)
	got := normalizeImage(llm.ImageInput{DataURL: src, Detail: "high"})
	after := dataURLRawBytes(got.DataURL)
	if after >= before/4 {
		t.Fatalf("payload shrank from %d to %d bytes, want at least a 4x reduction", before, after)
	}
	_, data, ok := splitImageDataURL(got.DataURL)
	if !ok {
		t.Fatal("result is not a data URL")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if max(cfg.Width, cfg.Height) > imageMaxEdgeDefaultPx {
		t.Fatalf("result long edge = %d, want <= %d", max(cfg.Width, cfg.Height), imageMaxEdgeDefaultPx)
	}
}

// TestNormalizeImageLeavesSmallImagesAlone pins the no-loss rule: an image
// already within the pixel and byte budget is returned byte-identical, so the
// common case costs nothing (and loses nothing).
func TestNormalizeImageLeavesSmallImagesAlone(t *testing.T) {
	src := pngDataURL(t, gradientImage(320, 200, false))
	if got := normalizeImage(llm.ImageInput{DataURL: src, Detail: "high"}); got.DataURL != src {
		t.Fatal("an in-budget image must be passed through unchanged")
	}
}

// TestNormalizeImageShrinksHugeBytesWithinPixelBudget covers the case that has
// nothing to do with pixels: a screenshot within the pixel budget whose PNG is
// still megabytes. It must be re-encoded smaller.
func TestNormalizeImageShrinksHugeBytesWithinPixelBudget(t *testing.T) {
	// Random-ish noise defeats PNG compression, so the bytes are large while
	// the dimensions stay under the pixel budget.
	img := image.NewRGBA(image.Rect(0, 0, 1400, 900))
	seed := uint32(12345)
	for y := 0; y < 900; y++ {
		for x := 0; x < 1400; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255})
		}
	}
	src := pngDataURL(t, img)
	if dataURLRawBytes(src) <= imageKeepBelowBytes {
		t.Skipf("fixture too small (%d bytes) to exercise the byte budget", dataURLRawBytes(src))
	}
	got := normalizeImage(llm.ImageInput{DataURL: src})
	if got.DataURL == src {
		t.Fatal("oversized payload within the pixel budget was not re-encoded")
	}
	if after := dataURLRawBytes(got.DataURL); after >= dataURLRawBytes(src) {
		t.Fatalf("payload grew: %d -> %d", dataURLRawBytes(src), after)
	}
}

// TestNormalizeImageCompositesTransparency pins the visual contract: a
// transparent PNG is flattened onto white (as a browser renders it) rather than
// turning black when re-encoded without an alpha channel.
func TestNormalizeImageCompositesTransparency(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2400, 1600))
	// Fully transparent except a red square: the flatten must produce white.
	for y := 0; y < 1600; y++ {
		for x := 0; x < 2400; x++ {
			if x > 1000 && x < 1400 && y > 600 && y < 1000 {
				img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
				continue
			}
			img.SetRGBA(x, y, color.RGBA{})
		}
	}
	got := normalizeImage(llm.ImageInput{DataURL: pngDataURL(t, img)})
	_, data, ok := splitImageDataURL(got.DataURL)
	if !ok {
		t.Fatal("result is not a data URL")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	r, g, b, _ := decoded.At(10, 10).RGBA()
	if r < 0xf0f0 || g < 0xf0f0 || b < 0xf0f0 {
		t.Fatalf("transparent area rendered as (%d,%d,%d), want near-white", r>>8, g>>8, b>>8)
	}
}

// TestNormalizeImageLeavesUnsupportedFormatsAlone pins the scope: formats the
// stdlib cannot decode (webp) and non-base64 data URLs are never touched.
func TestNormalizeImageLeavesUnsupportedFormatsAlone(t *testing.T) {
	cases := map[string]string{
		"webp":         "data:image/webp;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 50)),
		"bmp":          "data:image/bmp;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 50)),
		"url-encoded":  "data:image/png," + strings.Repeat("x", 50),
		"not an image": "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("hello")),
		"garbage":      "data:image/png;base64,!!!not base64!!!",
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			if got := normalizeImage(llm.ImageInput{DataURL: url, Detail: "low"}); got.DataURL != url {
				t.Fatalf("%s: image was modified", name)
			}
		})
	}
}

// TestNormalizeImageRespectsOffKnob pins the escape hatch: GOGEN_IMAGE_MAX_EDGE
// =off attaches images byte-for-byte.
func TestNormalizeImageRespectsOffKnob(t *testing.T) {
	t.Setenv("GOGEN_IMAGE_MAX_EDGE", "off")
	src := pngDataURL(t, gradientImage(3200, 2000, false))
	if got := normalizeImage(llm.ImageInput{DataURL: src}); got.DataURL != src {
		t.Fatal("downscaling must be disabled by GOGEN_IMAGE_MAX_EDGE=off")
	}
}

// TestNormalizeImageRespectsCustomEdge pins the tuning knob.
func TestNormalizeImageRespectsCustomEdge(t *testing.T) {
	t.Setenv("GOGEN_IMAGE_MAX_EDGE", "400")
	got := normalizeImage(llm.ImageInput{DataURL: pngDataURL(t, gradientImage(3200, 2000, false))})
	_, data, ok := splitImageDataURL(got.DataURL)
	if !ok {
		t.Fatal("result is not a data URL")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if max(cfg.Width, cfg.Height) > 400 {
		t.Fatalf("result long edge = %d, want <= 400", max(cfg.Width, cfg.Height))
	}
}

// TestNormalizeImagesSharedSliceSafety pins that the batch helper never mutates
// the caller's slice (the web server's validated copy is reused elsewhere).
func TestNormalizeImagesSharedSliceSafety(t *testing.T) {
	big := pngDataURL(t, gradientImage(3200, 2000, false))
	small := pngDataURL(t, gradientImage(32, 32, false))
	in := []llm.ImageInput{{DataURL: big, Detail: "auto"}, {DataURL: small, Detail: "low"}}

	out := normalizeImages(in)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d", len(out))
	}
	if in[0].DataURL != big || in[1].DataURL != small {
		t.Fatal("normalizeImages mutated the caller's slice")
	}
	if out[0].DataURL == big {
		t.Fatal("the oversized image was not normalized")
	}
	if out[1].DataURL != small {
		t.Fatal("the in-budget image must pass through unchanged")
	}
	if out[1].Detail != "low" {
		t.Fatalf("Detail = %q, want preserved", out[1].Detail)
	}
}
