//go:build !linux && !windows

package capture

import (
	"context"
	"errors"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

// IsWaylandSession returns false on non-Linux operating systems.
func IsWaylandSession() bool {
	return false
}

// GetShareableSources returns an empty list on unsupported platforms.
func GetShareableSources(context.Context) ([]domain.ShareableSource, error) {
	return nil, errors.New("screen sharing is only supported on Linux and Windows")
}

// NewPlatformCapturer returns an error on unsupported platforms.
func NewPlatformCapturer(CaptureConfig, *encoder.FramePool) (Capturer, error) {
	return nil, errors.New("screen capture is only supported on Linux and Windows")
}
