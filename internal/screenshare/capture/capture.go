package capture

import (
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"strings"
	"time"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

var (
	// ErrSourceClosed indicates the shared monitor, window, or OS portal permission was revoked/closed.
	ErrSourceClosed = errors.New("shared capture source closed")
	// ErrPortalCancelled indicates the user cancelled or denied the Wayland ScreenCast portal prompt.
	ErrPortalCancelled = errors.New("wayland screencast portal cancelled")
)

// FramePacket carries one captured RGBA frame and its pool release hook.
type FramePacket struct {
	Image      *image.RGBA
	CapturedAt time.Time
	Release    func()
}

// CaptureConfig specifies parameters for a platform screen/window capturer.
type CaptureConfig struct {
	SourceID           string
	Resolution         domain.ResolutionPreset
	FPS                int
	IncludeSystemAudio bool
	TargetWidth        int
	TargetHeight       int
}

// Capturer abstracts OS-specific screen/window video and desktop loopback audio capture.
type Capturer interface {
	Start(ctx context.Context) error
	Frames() <-chan FramePacket
	AudioFrames() <-chan []byte
	Err() error
	Close() error
}

// ThumbnailJPEGDataURL scales img proportionally within maxW x maxH and encodes it as a data:image/jpeg;base64 URL.
func ThumbnailJPEGDataURL(img image.Image, maxW, maxH int, pool *encoder.FramePool) (string, error) {
	if img == nil || img.Bounds().Empty() {
		return "", errors.New("empty image for thumbnail")
	}
	if maxW <= 0 {
		maxW = 320
	}
	if maxH <= 0 {
		maxH = 180
	}
	scaled := encoder.DownscaleRGBA(img, maxW, maxH, pool)
	if scaled == nil {
		return "", errors.New("failed to scale thumbnail")
	}
	defer pool.PutRGBA(scaled)

	jpg, err := encoder.EncodeRGBAToJPEG(scaled, 70)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg), nil
}

// GeneratePlaceholderThumbnail creates a crisp dark-themed fallback JPEG thumbnail when live capture preview is unavailable.
func GeneratePlaceholderThumbnail(title string, srcType domain.ShareSourceType, w, h int) string {
	const thumbW, thumbH = 320, 180
	img := image.NewRGBA(image.Rect(0, 0, thumbW, thumbH))

	bg := color.RGBA{R: 12, G: 15, B: 20, A: 255}
	panel := color.RGBA{R: 20, G: 25, B: 34, A: 255}
	accent := color.RGBA{R: 94, G: 194, B: 242, A: 255}
	if srcType == domain.SourceTypeScreen {
		accent = color.RGBA{R: 61, G: 220, B: 132, A: 255}
	}
	border := color.RGBA{R: 39, G: 48, B: 65, A: 255}

	for y := 0; y < thumbH; y++ {
		for x := 0; x < thumbW; x++ {
			c := bg
			if x == 0 || y == 0 || x == thumbW-1 || y == thumbH-1 {
				c = border
			} else if x >= 24 && x < thumbW-24 && y >= 22 && y < thumbH-22 {
				c = panel
				if y < 34 {
					c = border
				}
				if x >= 34 && x < 42 && y >= 25 && y <= 31 {
					c = accent
				}
			}
			img.SetRGBA(x, y, c)
		}
	}

	// Render a subtle deterministic hash bar representing source dimensions/title visually.
	hash := uint32(2166136261)
	for _, r := range strings.TrimSpace(title) {
		hash ^= uint32(r)
		hash *= 16777619
	}
	bars := 5 + int(hash%4)
	for i := 0; i < bars; i++ {
		by := 52 + i*14
		bw := 80 + int((hash>>(i*3))%120)
		if bw > 220 {
			bw = 220
		}
		for y := by; y < by+6 && y < thumbH-32; y++ {
			for x := 40; x < 40+bw && x < thumbW-40; x++ {
				if i == 0 {
					img.SetRGBA(x, y, accent)
				} else {
					img.SetRGBA(x, y, border)
				}
			}
		}
	}

	_ = w
	_ = h
	jpg, err := encoder.EncodeRGBAToJPEG(img, 70)
	if err != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg)
}
