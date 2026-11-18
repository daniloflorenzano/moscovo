package domain

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	// MinVideoBitrateBps is the minimum adaptive video bitrate (500 kbps).
	MinVideoBitrateBps = 500_000
	// DefaultVideoBitrateBps is the starting adaptive video bitrate (2 Mbps).
	DefaultVideoBitrateBps = 2_000_000
	// MaxVideoBitrateBps is the maximum adaptive video bitrate (4 Mbps).
	MaxVideoBitrateBps = 4_000_000
)

// ScreenShareState represents the concurrency-safe lifecycle state of local screen sharing.
type ScreenShareState string

const (
	ScreenShareIdle      ScreenShareState = "IDLE"
	ScreenShareStarting  ScreenShareState = "STARTING"
	ScreenShareStreaming ScreenShareState = "STREAMING"
	ScreenShareStopping  ScreenShareState = "STOPPING"
)

var allowedScreenShareTransitions = map[ScreenShareState][]ScreenShareState{
	ScreenShareIdle:      {ScreenShareStarting},
	ScreenShareStarting:  {ScreenShareStreaming, ScreenShareStopping, ScreenShareIdle},
	ScreenShareStreaming: {ScreenShareStopping, ScreenShareIdle},
	ScreenShareStopping:  {ScreenShareIdle},
}

// ValidateScreenShareTransition verifies whether moving from -> to is permitted.
func ValidateScreenShareTransition(from, to ScreenShareState) error {
	for _, candidate := range allowedScreenShareTransitions[from] {
		if candidate == to {
			return nil
		}
	}
	return fmt.Errorf("invalid screen share state transition %q -> %q", from, to)
}

// ScreenShareLifecycle guards screen-share state transitions against rapid-toggle race conditions.
type ScreenShareLifecycle struct {
	mu    sync.Mutex
	state ScreenShareState
}

// NewScreenShareLifecycle creates a lifecycle state guard initialized to ScreenShareIdle.
func NewScreenShareLifecycle() *ScreenShareLifecycle {
	return &ScreenShareLifecycle{state: ScreenShareIdle}
}

// State returns the current lifecycle state.
func (l *ScreenShareLifecycle) State() ScreenShareState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// TryStart atomically transitions IDLE -> STARTING.
func (l *ScreenShareLifecycle) TryStart() error {
	return l.transition(ScreenShareStarting)
}

// MarkStreaming atomically transitions STARTING -> STREAMING.
func (l *ScreenShareLifecycle) MarkStreaming() error {
	return l.transition(ScreenShareStreaming)
}

// BeginStop atomically transitions STARTING|STREAMING -> STOPPING.
func (l *ScreenShareLifecycle) BeginStop() error {
	return l.transition(ScreenShareStopping)
}

// MarkIdle atomically transitions STARTING|STREAMING|STOPPING -> IDLE.
func (l *ScreenShareLifecycle) MarkIdle() error {
	return l.transition(ScreenShareIdle)
}

// ResetIdle unconditionally restores the lifecycle to ScreenShareIdle after cleanup.
func (l *ScreenShareLifecycle) ResetIdle() {
	l.mu.Lock()
	l.state = ScreenShareIdle
	l.mu.Unlock()
}

func (l *ScreenShareLifecycle) transition(next ScreenShareState) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ValidateScreenShareTransition(l.state, next); err != nil {
		return err
	}
	l.state = next
	return nil
}

// ShareSourceType classifies a shareable desktop source.
type ShareSourceType string

const (
	SourceTypeScreen ShareSourceType = "screen"
	SourceTypeWindow ShareSourceType = "window"
)

// ShareableSource describes a monitor, window, or OS portal entry returned to the UI picker.
type ShareableSource struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Type      ShareSourceType `json:"type"`
	Thumbnail string          `json:"thumbnail"`
	Width     int             `json:"width,omitempty"`
	Height    int             `json:"height,omitempty"`
	IsPrimary bool            `json:"isPrimary,omitempty"`
}

// ResolutionPreset constrains the maximum video capture dimensions.
type ResolutionPreset string

const (
	Resolution720p   ResolutionPreset = "720p"
	Resolution1080p  ResolutionPreset = "1080p"
	ResolutionNative ResolutionPreset = "native"
)

// ScreenShareConfig defines user-selected parameters for starting a screen share stream.
type ScreenShareConfig struct {
	SourceID           string           `json:"sourceId"`
	Resolution         ResolutionPreset `json:"resolution"`
	FPS                int              `json:"fps"`
	IncludeSystemAudio bool             `json:"includeSystemAudio"`
}

// Normalized validates and returns a canonical ScreenShareConfig with defaults applied.
func (c ScreenShareConfig) Normalized() (ScreenShareConfig, error) {
	sourceID := strings.TrimSpace(c.SourceID)
	if sourceID == "" {
		return ScreenShareConfig{}, errors.New("screen share sourceId cannot be empty")
	}

	res := ResolutionPreset(strings.ToLower(strings.TrimSpace(string(c.Resolution))))
	if res == "" {
		res = Resolution720p
	}
	switch res {
	case Resolution720p, Resolution1080p, ResolutionNative:
	default:
		return ScreenShareConfig{}, fmt.Errorf("unsupported resolution preset %q", c.Resolution)
	}

	fps := c.FPS
	if fps == 0 {
		fps = 30
	}
	switch fps {
	case 15, 30, 60:
	default:
		return ScreenShareConfig{}, fmt.Errorf("unsupported target fps %d (must be 15, 30, or 60)", fps)
	}

	return ScreenShareConfig{
		SourceID:           sourceID,
		Resolution:         res,
		FPS:                fps,
		IncludeSystemAudio: c.IncludeSystemAudio,
	}, nil
}

// Validate verifies that the screen-share configuration satisfies all domain invariants.
func (c ScreenShareConfig) Validate() error {
	_, err := c.Normalized()
	return err
}

// ResolveTargetDimensions fits source dimensions within preset bounds without upscaling
// and rounds width and height down to even numbers (>= 2) for YUV420p encoding.
func ResolveTargetDimensions(srcW, srcH int, preset ResolutionPreset) (int, int) {
	var maxW, maxH int
	switch ResolutionPreset(strings.ToLower(strings.TrimSpace(string(preset)))) {
	case Resolution1080p:
		maxW, maxH = 1920, 1080
	case ResolutionNative:
		if srcW > 0 && srcH > 0 {
			return roundEvenMin2(srcW), roundEvenMin2(srcH)
		}
		return 1920, 1080
	default:
		maxW, maxH = 1280, 720
	}

	if srcW <= 0 || srcH <= 0 {
		return maxW, maxH
	}
	if srcW <= maxW && srcH <= maxH {
		return roundEvenMin2(srcW), roundEvenMin2(srcH)
	}

	// Preserve aspect ratio using floating scale factor bounded by the tighter axis.
	scaleW := float64(maxW) / float64(srcW)
	scaleH := float64(maxH) / float64(srcH)
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}
	dstW := roundEvenMin2(int(float64(srcW)*scale + 0.5))
	dstH := roundEvenMin2(int(float64(srcH)*scale + 0.5))
	if dstW > maxW {
		dstW = roundEvenMin2(maxW)
	}
	if dstH > maxH {
		dstH = roundEvenMin2(maxH)
	}
	return dstW, dstH
}

func roundEvenMin2(v int) int {
	if v <= 2 {
		return 2
	}
	return v &^ 1
}

// ComputeAdaptiveBitrate adjusts video encoder target bitrate within [MinVideoBitrateBps, MaxVideoBitrateBps]
// based on REMB feedback, packet loss fraction, and RTT latency.
func ComputeAdaptiveBitrate(currentBps int, rembBps uint64, lossFraction float64, rttMs int) int {
	next := currentBps
	if next <= 0 {
		next = DefaultVideoBitrateBps
	}

	if lossFraction > 0.05 || rttMs > 250 {
		// Cut target bitrate by 25% under congestion or elevated RTT.
		next = (next * 3) / 4
	} else if lossFraction < 0.02 && (rttMs <= 0 || rttMs < 150) {
		// Ramp up gently by 8% on healthy links.
		next = (next * 108) / 100
	}

	if rembBps > 0 && int64(rembBps) < int64(next) {
		next = int(rembBps)
	}

	if next < MinVideoBitrateBps {
		return MinVideoBitrateBps
	}
	if next > MaxVideoBitrateBps {
		return MaxVideoBitrateBps
	}
	return next
}
