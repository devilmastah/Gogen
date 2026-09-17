package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // registers the gif decoder for DecodeConfig sniffing
	"image/jpeg"
	_ "image/png" // registers the png decoder
	"os"
	"strconv"
	"strings"

	"gogen/internal/llm"
	"gogen/internal/onoff"
)

// Attached images are re-uploaded on EVERY subsequent request of a session:
// chat completions is stateless, so the whole conversation (including every
// data URL) rides on each round. A 3.5 MB screenshot therefore costs ~4.7 MB
// of base64 per round, and a long agent loop with a handful of screenshots can
// upload hundreds of megabytes through an endpoint that may already be
// serializing generations.
//
// Downscaling at ingest is the fix that costs nothing functionally: vision
// models resize internally to roughly 1–1.5k pixels on the long edge, so an
// image above that budget carries no information the model can use — it only
// costs bytes and prefill time. The default budget below matches the common
// vision-model working resolution.
//
// Only round-trippable formats are touched (PNG and JPEG). WebP/BMP have no
// stdlib decoder, and GIF may be animated, so those pass through unchanged
// rather than being silently degraded.
const (
	// imageMaxEdgeDefaultPx is the long-edge pixel budget for an attached
	// image. Vision pipelines tile/resize to roughly this size; anything
	// larger is bytes the model never sees.
	imageMaxEdgeDefaultPx = 1568
	// imageKeepBelowBytes is where the byte budget stops being about pixels:
	// below it, an image within the pixel budget is left completely alone
	// (no re-encode, no quality loss).
	imageKeepBelowBytes = 600 * 1024
	// imageJPEGQuality is the re-encode quality for downscaled images.
	imageJPEGQuality = 85
	// imageTargetBytes is the payload budget the re-encode retries toward.
	imageTargetBytes = 1 << 20
	// imageLadderEdge/Quality are the fallback rung when the first encode is
	// still over budget.
	imageLadderEdge    = 1024
	imageLadderQuality = 70
)

// imageMaxEdge returns the configured long-edge pixel budget (0 = leave every
// image untouched). GOGEN_IMAGE_MAX_EDGE overrides the default; 0/off disables
// downscaling entirely for users who want byte-exact attachments.
func imageMaxEdge() int {
	raw := strings.TrimSpace(os.Getenv("GOGEN_IMAGE_MAX_EDGE"))
	if raw == "" {
		return imageMaxEdgeDefaultPx
	}
	if on, ok := onoff.Parse(raw); ok && !on {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return imageMaxEdgeDefaultPx
	}
	return n
}

// normalizeImage shrinks one attached image to the pixel/byte budget so the
// session stops re-uploading the original bytes on every round. Images already
// within budget are returned unchanged (no decode cost beyond the header), as
// are formats the stdlib cannot re-encode and anything that would grow.
func normalizeImage(img llm.ImageInput) llm.ImageInput {
	maxEdge := imageMaxEdge()
	if maxEdge <= 0 || img.DataURL == "" {
		return img
	}
	mime, data, ok := splitImageDataURL(img.DataURL)
	if !ok || data == nil {
		return img
	}
	switch mime {
	case "image/png", "image/jpeg":
	default:
		return img
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return img
	}
	longEdge := max(cfg.Width, cfg.Height)
	if longEdge <= maxEdge && len(data) <= imageKeepBelowBytes {
		return img
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return img
	}
	if out, ok := reencodeJPEG(src, maxEdge, imageJPEGQuality); ok && len(out) < len(data) {
		img.DataURL = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(out)
		return img
	}
	// First encode did not beat the original (or was still over budget):
	// try one more aggressive rung before giving up on the original bytes.
	if longEdge > imageLadderEdge || len(data) > imageTargetBytes {
		if out, ok := reencodeJPEG(src, min(maxEdge, imageLadderEdge), imageLadderQuality); ok && len(out) < len(data) {
			img.DataURL = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(out)
		}
	}
	return img
}

// normalizeImages applies normalizeImage to every image of a user message.
// Returns the input slice untouched when nothing changed.
func normalizeImages(images []llm.ImageInput) []llm.ImageInput {
	for i, img := range images {
		normalized := normalizeImage(img)
		if normalized.DataURL != img.DataURL {
			out := append([]llm.ImageInput(nil), images...)
			out[i] = normalized
			for j := i + 1; j < len(images); j++ {
				out[j] = normalizeImage(images[j])
			}
			return out
		}
	}
	return images
}

// reencodeJPEG scales src to fit maxEdge on its long edge and encodes it as
// JPEG. A non-opaque source is composited onto white first, matching what a
// browser shows for an image without a background (JPEG has no alpha channel).
func reencodeJPEG(src image.Image, maxEdge, quality int) ([]byte, bool) {
	if maxEdge <= 0 {
		return nil, false
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, false
	}
	scale := min(1.0, float64(maxEdge)/float64(max(sw, sh)))
	dw := max(1, int(float64(sw)*scale))
	dh := max(1, int(float64(sh)*scale))

	var base image.Image = src
	if o, ok := src.(interface{ Opaque() bool }); ok && !o.Opaque() {
		flat := image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(flat, flat.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), src, b.Min, draw.Over)
		base = flat
	}

	scaled := downscaleArea(base, dw, dh)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// downscaleArea averages each destination pixel over its source rectangle (a
// box filter), the standard way to shrink an image without aliasing. Averaging
// is done on premultiplied RGBA values, which is what image.RGBA stores, so the
// result stays exact for opaque images and correct for composited ones.
func downscaleArea(src image.Image, dstW, dstH int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for dy := 0; dy < dstH; dy++ {
		y0 := b.Min.Y + dy*sh/dstH
		y1 := b.Min.Y + (dy+1)*sh/dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			x0 := b.Min.X + dx*sw/dstW
			x1 := b.Min.X + (dx+1)*sw/dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as, n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bb, a := src.At(x, y).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(bb)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(rs / n >> 8),
				G: uint8(gs / n >> 8),
				B: uint8(bs / n >> 8),
				A: uint8(as / n >> 8),
			})
		}
	}
	return dst
}

// splitImageDataURL parses "data:<mime>;base64,<payload>" into its MIME type and
// decoded bytes. ok is false for anything that is not a base64 image data URL.
func splitImageDataURL(url string) (mime string, data []byte, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", nil, false
	}
	rest := url[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", nil, false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", nil, false
	}
	mime = strings.TrimSuffix(meta, ";base64")
	if !strings.HasPrefix(mime, "image/") {
		return "", nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return mime, decoded, true
}
