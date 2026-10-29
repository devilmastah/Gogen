package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// readImageMaxBytes caps the raw file size read_image will attach. Base64
// inflates the payload ~33%, so a 3.5 MB raw file becomes a ~4.7 MB data URL
// — comfortably under the server's 5 MB user-attached-image cap
// (maxImageDataURLBytes in internal/server/ws_types.go), which the agent
// package deliberately does not import.
const readImageMaxBytes = 3_500_000

// maxSessionImages is the soft cap on images read_image may attach per
// session. Image bytes are the most expensive content in context: every
// subsequent API request resends all attached data URLs, so the cap keeps a
// session from filling its window with screenshots.
const maxSessionImages = 8

// imageExtMIMEs maps file extensions to MIME types for files whose content
// sniffing is inconclusive (http.DetectContentType returns
// application/octet-stream for a few small or unusual encodings).
var imageExtMIMEs = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// handleReadImage validates an image file and attaches it to the session as
// a synthetic user message. The handler itself never touches the transcript:
// it validates, reads, and encodes the file, then records the image in the
// context-scoped image sink; the tool-round coordinator appends the user
// message after ALL tool messages of the round (see appendRoundImages) —
// never between them, which strict providers reject. The returned text is a
// short ack — the image itself travels in the user message.
func handleReadImage(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	detail, err := stringArgOptional(args, "detail")
	if err != nil {
		return "", err
	}
	switch detail {
	case "", "auto", "low", "high":
	default:
		return "", fmt.Errorf("invalid detail %q (use auto, low, or high)", detail)
	}

	secure, size, err := a.Executor.validateImageFile(path)
	if err != nil {
		return "", err
	}
	if size > readImageMaxBytes {
		return "", fmt.Errorf("image is %s (%d bytes); read_image supports files up to %s. Crop or resize it first",
			formatByteSize(size), size, formatByteSize(readImageMaxBytes))
	}

	data, err := os.ReadFile(secure)
	if err != nil {
		return "", err
	}
	mime := sniffImageMIME(data, secure)
	if mime == "" {
		return "", fmt.Errorf("%s is not a supported image (detected %q); read_image supports png, jpeg, gif, and webp",
			path, http.DetectContentType(headBytes(data)))
	}
	if mime == "image/svg+xml" {
		return "", fmt.Errorf("SVG files are XML text, not raster images; use read_file to view the source instead")
	}

	sink := ImageSinkFromContext(ctx)
	if sink == nil {
		return "", fmt.Errorf("read_image: no image sink attached to this tool call (internal error)")
	}
	if !a.reserveImageSlot() {
		return "", fmt.Errorf("session image limit reached (%d images). Images are the most expensive content in context — attach only what the current task needs, or continue in a fresh session", maxSessionImages)
	}

	displayDetail := detail
	if displayDetail == "" {
		displayDetail = "auto"
	}
	original := llm.ImageInput{
		DataURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		Detail:  detail,
	}
	// Downscale before the image becomes part of the transcript: every
	// subsequent request re-uploads it, so the bytes attached here are paid
	// again on every round (see image_scale.go).
	attached := normalizeImage(original)
	attachedSize := formatByteSize(size)
	if attached.DataURL != original.DataURL {
		attachedSize = formatByteSize(int64(dataURLRawBytes(attached.DataURL)))
	}
	sink.Add(attached, contextmgr.ReadImagePrefix+fmt.Sprintf("%s — %s, %s", path, mime, attachedSize))
	if attached.DataURL != original.DataURL {
		return fmt.Sprintf("Image attached to context: %s (%s, %s downscaled from %s, detail=%s)",
			path, mime, attachedSize, formatByteSize(size), displayDetail), nil
	}
	return fmt.Sprintf("Image attached to context: %s (%s, %s, detail=%s)", path, mime, attachedSize, displayDetail), nil
}

// dataURLRawBytes returns the decoded payload size of a base64 data URL: every
// four encoded characters carry three bytes (padding included).
func dataURLRawBytes(url string) int {
	i := strings.IndexByte(url, ',')
	if i < 0 {
		return 0
	}
	return (len(url) - i - 1) / 4 * 3
}

// countImages returns the number of read_image attachments across msgs.
// Used to re-derive the per-session image budget whenever the message list
// is replaced or truncated wholesale (compaction, restore, fork, reset,
// rollback): attachments that survive keep counting, dropped ones release
// their slots. Without this the counter only ever grows.
func countImages(msgs []llm.Message) int32 {
	var n int32
	for i := range msgs {
		n += int32(len(msgs[i].Images))
	}
	return n
}

// reserveImageSlot claims one slot of the per-session image budget
// (maxSessionImages). read_image handlers can run concurrently in parallel
// read-only batches, so the counter is atomic and the claim is a CAS loop.
func (a *Agent) reserveImageSlot() bool {
	for {
		cur := a.sessionImages.Load()
		if cur >= maxSessionImages {
			return false
		}
		if a.sessionImages.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// headBytes returns the first 512 bytes of data — the sniff window
// http.DetectContentType uses — or the whole slice when shorter.
func headBytes(data []byte) []byte {
	if len(data) > 512 {
		return data[:512]
	}
	return data
}

// sniffImageMIME determines the MIME type of an image file: content sniffing
// first, extension fallback when sniffing is inconclusive. Returns "" for
// non-image content. SVG is special-cased: http.DetectContentType does not
// reliably identify it by magic bytes (it may report text/xml or
// text/plain), so the .svg extension is the signal that turns those into
// image/svg+xml — which the handler then rejects with a read_file hint.
func sniffImageMIME(data []byte, securePath string) string {
	mime := http.DetectContentType(headBytes(data))
	if strings.HasPrefix(mime, "image/") {
		return mime
	}
	ext := strings.ToLower(filepath.Ext(securePath))
	if mime == "application/octet-stream" {
		if m, ok := imageExtMIMEs[ext]; ok {
			return m
		}
	}
	// DetectContentType may report SVG-ish XML/plain content as
	// "text/plain; charset=utf-8" or "text/xml; charset=utf-8" (it has no
	// reliable SVG magic), so the .svg extension is the signal that turns
	// text-family detections into image/svg+xml — which the handler then
	// rejects with a read_file hint.
	if ext == ".svg" && strings.HasPrefix(mime, "text/") {
		return "image/svg+xml"
	}
	return ""
}

// validateImageFile checks that path resolves inside the workspace boundary
// and names a regular file, returning the secure absolute path and its size.
// Unlike validateAndCheckFile it does NOT reject binary content — images are
// exactly binary — and it returns no read header, because read_image
// attaches the raw bytes instead of rendering text.
func (e *Executor) validateImageFile(path string) (string, int64, error) {
	secure, err := e.SecurePath(path)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(secure)
	if err != nil {
		return "", 0, err
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("path is a directory; read_image needs a file path")
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("path is not a regular file")
	}
	return secure, info.Size(), nil
}
