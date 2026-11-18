package domain

import (
	"sync"
	"testing"
)

func TestScreenShareStateTransitions(t *testing.T) {
	valid := []struct {
		from ScreenShareState
		to   ScreenShareState
	}{
		{ScreenShareIdle, ScreenShareStarting},
		{ScreenShareStarting, ScreenShareStreaming},
		{ScreenShareStarting, ScreenShareStopping},
		{ScreenShareStarting, ScreenShareIdle},
		{ScreenShareStreaming, ScreenShareStopping},
		{ScreenShareStreaming, ScreenShareIdle},
		{ScreenShareStopping, ScreenShareIdle},
	}

	for _, tc := range valid {
		if err := ValidateScreenShareTransition(tc.from, tc.to); err != nil {
			t.Fatalf("expected valid transition %q -> %q, got error: %v", tc.from, tc.to, err)
		}
	}

	invalid := []struct {
		from ScreenShareState
		to   ScreenShareState
	}{
		{ScreenShareIdle, ScreenShareIdle},
		{ScreenShareIdle, ScreenShareStreaming},
		{ScreenShareIdle, ScreenShareStopping},
		{ScreenShareStarting, ScreenShareStarting},
		{ScreenShareStreaming, ScreenShareStarting},
		{ScreenShareStreaming, ScreenShareStreaming},
		{ScreenShareStopping, ScreenShareStarting},
		{ScreenShareStopping, ScreenShareStreaming},
		{ScreenShareStopping, ScreenShareStopping},
		{ScreenShareState("UNKNOWN"), ScreenShareStarting},
	}

	for _, tc := range invalid {
		if err := ValidateScreenShareTransition(tc.from, tc.to); err == nil {
			t.Fatalf("expected invalid transition %q -> %q to return error", tc.from, tc.to)
		}
	}
}

func TestScreenShareLifecycleConcurrency(t *testing.T) {
	lc := NewScreenShareLifecycle()
	if lc.State() != ScreenShareIdle {
		t.Fatalf("initial state = %q, want %q", lc.State(), ScreenShareIdle)
	}

	if err := lc.MarkStreaming(); err == nil {
		t.Fatalf("expected MarkStreaming() from IDLE to fail")
	}

	// Only one concurrent caller should win TryStart().
	const workers = 32
	var wg sync.WaitGroup
	wins := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wins <- lc.TryStart() == nil
		}()
	}
	wg.Wait()
	close(wins)

	winCount := 0
	for won := range wins {
		if won {
			winCount++
		}
	}
	if winCount != 1 {
		t.Fatalf("concurrent TryStart winners = %d, want 1", winCount)
	}
	if lc.State() != ScreenShareStarting {
		t.Fatalf("state after TryStart = %q, want %q", lc.State(), ScreenShareStarting)
	}

	if err := lc.MarkStreaming(); err != nil {
		t.Fatalf("MarkStreaming() failed: %v", err)
	}
	if lc.State() != ScreenShareStreaming {
		t.Fatalf("state after MarkStreaming = %q, want %q", lc.State(), ScreenShareStreaming)
	}

	// Only one concurrent caller should win BeginStop().
	stopWins := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stopWins <- lc.BeginStop() == nil
		}()
	}
	wg.Wait()
	close(stopWins)

	stopWinCount := 0
	for won := range stopWins {
		if won {
			stopWinCount++
		}
	}
	if stopWinCount != 1 {
		t.Fatalf("concurrent BeginStop winners = %d, want 1", stopWinCount)
	}
	if lc.State() != ScreenShareStopping {
		t.Fatalf("state after BeginStop = %q, want %q", lc.State(), ScreenShareStopping)
	}

	if err := lc.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle() failed: %v", err)
	}
	if lc.State() != ScreenShareIdle {
		t.Fatalf("state after MarkIdle = %q, want %q", lc.State(), ScreenShareIdle)
	}

	// Idempotent MarkIdle from IDLE is rejected by transition validator or ResetIdle works cleanly.
	if err := lc.TryStart(); err != nil {
		t.Fatalf("second TryStart() after MarkIdle failed: %v", err)
	}
	lc.ResetIdle()
	if lc.State() != ScreenShareIdle {
		t.Fatalf("state after ResetIdle = %q, want %q", lc.State(), ScreenShareIdle)
	}
}

func TestScreenShareConfigValidation(t *testing.T) {
	tests := []struct {
		name     string
		cfg      ScreenShareConfig
		wantErr  bool
		wantRes  ResolutionPreset
		wantFPS  int
		wantAud  bool
		wantSrc  string
	}{
		{
			name: "defaults applied for empty resolution and zero fps",
			cfg: ScreenShareConfig{
				SourceID: "screen:0",
			},
			wantRes: Resolution720p,
			wantFPS: 30,
			wantSrc: "screen:0",
		},
		{
			name: "1080p 60fps with system audio",
			cfg: ScreenShareConfig{
				SourceID:           "window:0x123",
				Resolution:         ResolutionPreset("1080P"),
				FPS:                60,
				IncludeSystemAudio: true,
			},
			wantRes: Resolution1080p,
			wantFPS: 60,
			wantAud: true,
			wantSrc: "window:0x123",
		},
		{
			name: "Native mixed-case normalized at 15fps",
			cfg: ScreenShareConfig{
				SourceID:   "wayland:portal",
				Resolution: ResolutionPreset("Native"),
				FPS:        15,
			},
			wantRes: ResolutionNative,
			wantFPS: 15,
			wantSrc: "wayland:portal",
		},
		{
			name: "missing source id rejected",
			cfg: ScreenShareConfig{
				SourceID:   "   ",
				Resolution: Resolution720p,
				FPS:        30,
			},
			wantErr: true,
		},
		{
			name: "invalid resolution rejected",
			cfg: ScreenShareConfig{
				SourceID:   "screen:0",
				Resolution: ResolutionPreset("4k"),
				FPS:        30,
			},
			wantErr: true,
		},
		{
			name: "unsupported fps rejected",
			cfg: ScreenShareConfig{
				SourceID:   "screen:0",
				Resolution: Resolution720p,
				FPS:        24,
			},
			wantErr: true,
		},
		{
			name: "negative fps rejected",
			cfg: ScreenShareConfig{
				SourceID:   "screen:0",
				Resolution: Resolution720p,
				FPS:        -15,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			normalized, err := tc.cfg.Normalized()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Normalized() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if normalized.SourceID != tc.wantSrc {
				t.Fatalf("SourceID = %q, want %q", normalized.SourceID, tc.wantSrc)
			}
			if normalized.Resolution != tc.wantRes {
				t.Fatalf("Resolution = %q, want %q", normalized.Resolution, tc.wantRes)
			}
			if normalized.FPS != tc.wantFPS {
				t.Fatalf("FPS = %d, want %d", normalized.FPS, tc.wantFPS)
			}
			if normalized.IncludeSystemAudio != tc.wantAud {
				t.Fatalf("IncludeSystemAudio = %v, want %v", normalized.IncludeSystemAudio, tc.wantAud)
			}
		})
	}
}

func TestResolveTargetDimensions(t *testing.T) {
	tests := []struct {
		name   string
		srcW   int
		srcH   int
		preset ResolutionPreset
		wantW  int
		wantH  int
	}{
		{
			name:   "4k downscaled to 1080p",
			srcW:   3840,
			srcH:   2160,
			preset: Resolution1080p,
			wantW:  1920,
			wantH:  1080,
		},
		{
			name:   "1080p downscaled to 720p",
			srcW:   1920,
			srcH:   1080,
			preset: Resolution720p,
			wantW:  1280,
			wantH:  720,
		},
		{
			name:   "small window never upscaled for 1080p",
			srcW:   801,
			srcH:   601,
			preset: Resolution1080p,
			wantW:  800,
			wantH:  600,
		},
		{
			name:   "ultrawide monitor fits within 720p bounding box with even dims",
			srcW:   3440,
			srcH:   1440,
			preset: Resolution720p,
			wantW:  1280,
			wantH:  536,
		},
		{
			name:   "native preserves source rounded down to even",
			srcW:   2559,
			srcH:   1439,
			preset: ResolutionNative,
			wantW:  2558,
			wantH:  1438,
		},
		{
			name:   "portrait window fits height bound of 720p",
			srcW:   900,
			srcH:   1600,
			preset: Resolution720p,
			wantW:  404,
			wantH:  720,
		},
		{
			name:   "zero source falls back to preset default",
			srcW:   0,
			srcH:   0,
			preset: Resolution720p,
			wantW:  1280,
			wantH:  720,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, h := ResolveTargetDimensions(tc.srcW, tc.srcH, tc.preset)
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("ResolveTargetDimensions(%d, %d, %q) = (%d, %d), want (%d, %d)",
					tc.srcW, tc.srcH, tc.preset, w, h, tc.wantW, tc.wantH)
			}
			if w%2 != 0 || h%2 != 0 {
				t.Fatalf("dimensions (%d, %d) must be even for YUV420p", w, h)
			}
		})
	}
}

func TestComputeAdaptiveBitrate(t *testing.T) {
	tests := []struct {
		name     string
		current  int
		remb     uint64
		loss     float64
		rttMs    int
		wantMin  int
		wantMax  int
	}{
		{
			name:    "clamps below min bitrate",
			current: 200_000,
			remb:    0,
			loss:    0,
			rttMs:   50,
			wantMin: MinVideoBitrateBps,
			wantMax: 600_000,
		},
		{
			name:    "caps immediately to REMB",
			current: 3_000_000,
			remb:    1_200_000,
			loss:    0.01,
			rttMs:   40,
			wantMin: 1_100_000,
			wantMax: 1_200_000,
		},
		{
			name:    "REMB below floor still respects min bitrate floor",
			current: 1_500_000,
			remb:    100_000,
			loss:    0,
			rttMs:   30,
			wantMin: MinVideoBitrateBps,
			wantMax: MinVideoBitrateBps,
		},
		{
			name:    "high packet loss cuts bitrate by 20-35 percent",
			current: 2_000_000,
			remb:    0,
			loss:    0.10,
			rttMs:   80,
			wantMin: 1_300_000,
			wantMax: 1_600_000,
		},
		{
			name:    "high rtt cuts bitrate",
			current: 2_000_000,
			remb:    0,
			loss:    0.01,
			rttMs:   300,
			wantMin: 1_300_000,
			wantMax: 1_600_000,
		},
		{
			name:    "healthy link ramps bitrate gently up to max",
			current: 2_000_000,
			remb:    0,
			loss:    0.0,
			rttMs:   40,
			wantMin: 2_100_000,
			wantMax: 2_200_000,
		},
		{
			name:    "never exceeds max video bitrate",
			current: 4_000_000,
			remb:    10_000_000,
			loss:    0.0,
			rttMs:   20,
			wantMin: MaxVideoBitrateBps,
			wantMax: MaxVideoBitrateBps,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeAdaptiveBitrate(tc.current, tc.remb, tc.loss, tc.rttMs)
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("ComputeAdaptiveBitrate(%d, %d, %.3f, %d) = %d, want in [%d, %d]",
					tc.current, tc.remb, tc.loss, tc.rttMs, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}
