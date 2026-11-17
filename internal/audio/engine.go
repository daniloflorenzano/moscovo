package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultFrameDuration is the standard 20ms Opus ptime.
	DefaultFrameDuration = 20 * time.Millisecond
	// SampleRate is the 48kHz Opus wideband clock rate.
	SampleRate = 48000
	// speakingThreshold sets the minimum normalized RMS level treated as speech.
	speakingThreshold = 0.02
	// speakingHangover keeps the speaking indicator steady across short pauses.
	speakingHangover = 250 * time.Millisecond
)

// ErrEngineClosed is returned when reading from a stopped AudioIO engine.
var ErrEngineClosed = errors.New("audio engine closed")

// opusSilenceFrame is a valid 20ms RFC 6716 Opus DTX/silence frame (48kHz mono).
var opusSilenceFrame = []byte{0xF8, 0xFF, 0xFE}

// AudioSample represents one encoded audio frame captured from the mic or received from a peer.
type AudioSample struct {
	Data      []byte
	Duration  time.Duration
	Timestamp time.Time
	Energy    float64
	Sequence  uint64
}

// PeerAudioStats tracks incoming audio telemetry for a single remote peer.
type PeerAudioStats struct {
	PacketsReceived uint64
	BytesReceived   uint64
	Speaking        bool
	RMSLevel        float64
	LastActive      time.Time
	LastSpoke       time.Time
}

// AudioIO decouples WebRTC transport from hardware microphone and speaker backends.
type AudioIO interface {
	Start(ctx context.Context) error
	ReadMicSample(ctx context.Context) (AudioSample, error)
	WritePeerSample(peerID string, sample AudioSample, volume int, muted bool) error
	Close() error
}

// EngineConfig configures the audio capture/mixer engine.
type EngineConfig struct {
	FrameDuration    time.Duration
	MicSource        func() (payload []byte, rms float64)
	PlaybackSink     func(peerID string, sample AudioSample, volume int)
	OnSpeakingChange func(speaking bool)
}

// Engine implements AudioIO with 20ms frame pacing, mute/deafen/PTT state, and per-peer stats.
type Engine struct {
	frameDuration    time.Duration
	micSource        func() ([]byte, float64)
	playbackSink     func(peerID string, sample AudioSample, volume int)
	onSpeakingChange func(bool)

	mu            sync.RWMutex
	started       bool
	closed        bool
	muted         bool
	deafened      bool
	pushToTalk    bool
	speaking      bool
	speakingUntil time.Time
	seq           uint64
	peerStats     map[string]*PeerAudioStats
	platformPlay  func([]byte)
	platformClose func()

	closeCh   chan struct{}
	closeOnce sync.Once
	tickerMu  sync.Mutex
	ticker    *time.Ticker
	running   atomic.Bool
}

// NewEngine creates an audio Engine with sensible 20ms defaults.
func NewEngine(cfg EngineConfig) *Engine {
	dur := cfg.FrameDuration
	if dur <= 0 {
		dur = DefaultFrameDuration
	}
	return &Engine{
		frameDuration:    dur,
		micSource:        cfg.MicSource,
		playbackSink:     cfg.PlaybackSink,
		onSpeakingChange: cfg.OnSpeakingChange,
		peerStats:        make(map[string]*PeerAudioStats),
		closeCh:          make(chan struct{}),
	}
}

// Start activates frame pacing and opens platform audio streams when no custom MicSource is injected.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrEngineClosed
	}
	if e.started {
		e.mu.Unlock()
		return nil
	}
	e.started = true
	usePlatform := e.micSource == nil
	e.mu.Unlock()

	if usePlatform {
		streamCtx := ctx
		if streamCtx == nil {
			streamCtx = context.Background()
		}
		micRead, playWrite, closeFn := openPlatformStream(streamCtx)
		e.mu.Lock()
		if !e.closed {
			if micRead != nil && e.micSource == nil {
				e.micSource = micRead
			}
			e.platformPlay = playWrite
			e.platformClose = closeFn
		} else if closeFn != nil {
			closeFn()
		}
		e.mu.Unlock()
	}

	e.tickerMu.Lock()
	if e.ticker == nil {
		e.ticker = time.NewTicker(e.frameDuration)
	}
	e.tickerMu.Unlock()

	e.running.Store(true)
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = e.Close()
			case <-e.closeCh:
			}
		}()
	}
	return nil
}

// ReadMicSample waits for the next 20ms frame tick and returns an audio sample.
func (e *Engine) ReadMicSample(ctx context.Context) (AudioSample, error) {
	if !e.running.Load() {
		if err := e.Start(ctx); err != nil {
			return AudioSample{}, err
		}
	}

	e.tickerMu.Lock()
	ticker := e.ticker
	e.tickerMu.Unlock()
	if ticker == nil {
		return AudioSample{}, ErrEngineClosed
	}

	select {
	case <-ctx.Done():
		return AudioSample{}, ctx.Err()
	case <-e.closeCh:
		return AudioSample{}, ErrEngineClosed
	case now := <-ticker.C:
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return AudioSample{}, ErrEngineClosed
		}
		e.seq++

		effMuted := e.isEffectivelyMutedLocked()
		payload := append([]byte(nil), opusSilenceFrame...)
		rms := 0.0
		if !effMuted && e.micSource != nil {
			customPayload, customRMS := e.micSource()
			if len(customPayload) > 0 {
				payload = append([]byte(nil), customPayload...)
			}
			rms = customRMS
		}

		if !effMuted && rms >= speakingThreshold {
			e.speakingUntil = now.Add(speakingHangover)
		}
		nextSpeaking := !effMuted && (rms >= speakingThreshold || now.Before(e.speakingUntil))
		changed := nextSpeaking != e.speaking
		e.speaking = nextSpeaking
		cb := e.onSpeakingChange
		seq := e.seq
		e.mu.Unlock()

		if changed && cb != nil {
			cb(nextSpeaking)
		}

		return AudioSample{
			Data:      payload,
			Duration:  e.frameDuration,
			Timestamp: now,
			Energy:    rms,
			Sequence:  seq,
		}, nil
	}
}

// WritePeerSample ingests a received peer audio frame, updating telemetry and playback.
func (e *Engine) WritePeerSample(peerID string, sample AudioSample, volume int, muted bool) error {
	now := time.Now()
	rms := sample.Energy
	if rms == 0 && len(sample.Data) >= 64 {
		rms = ComputePCM16RMS(sample.Data)
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrEngineClosed
	}
	deafened := e.deafened
	stats, ok := e.peerStats[peerID]
	if !ok {
		stats = &PeerAudioStats{}
		e.peerStats[peerID] = stats
	}
	stats.PacketsReceived++
	stats.BytesReceived += uint64(len(sample.Data))
	stats.LastActive = now
	stats.RMSLevel = rms
	if rms >= speakingThreshold {
		stats.LastSpoke = now
	}
	stats.Speaking = rms >= speakingThreshold || now.Sub(stats.LastSpoke) <= speakingHangover
	sink := e.playbackSink
	platformPlay := e.platformPlay
	e.mu.Unlock()

	if deafened || muted || volume <= 0 {
		return nil
	}
	if sink != nil {
		sink(peerID, sample, volume)
	}
	if platformPlay != nil && len(sample.Data) >= 64 {
		platformPlay(ScalePCM16(sample.Data, volume))
	}
	return nil
}

// SetMuted sets the user's microphone mute toggle.
func (e *Engine) SetMuted(muted bool) {
	e.mu.Lock()
	e.muted = muted
	changed, next, cb := e.syncSpeakingAfterToggleLocked()
	e.mu.Unlock()
	if changed && cb != nil {
		cb(next)
	}
}

// IsMuted reports the raw mute toggle state.
func (e *Engine) IsMuted() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.muted
}

// SetDeafened enables or disables output deafen (which also mutes mic input).
func (e *Engine) SetDeafened(deafened bool) {
	e.mu.Lock()
	e.deafened = deafened
	changed, next, cb := e.syncSpeakingAfterToggleLocked()
	e.mu.Unlock()
	if changed && cb != nil {
		cb(next)
	}
}

// IsDeafened reports whether audio output is deafened.
func (e *Engine) IsDeafened() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.deafened
}

// SetPushToTalk activates or releases push-to-talk override while muted.
func (e *Engine) SetPushToTalk(active bool) {
	e.mu.Lock()
	e.pushToTalk = active
	changed, next, cb := e.syncSpeakingAfterToggleLocked()
	e.mu.Unlock()
	if changed && cb != nil {
		cb(next)
	}
}

func (e *Engine) syncSpeakingAfterToggleLocked() (bool, bool, func(bool)) {
	if e.isEffectivelyMutedLocked() && e.speaking {
		e.speaking = false
		e.speakingUntil = time.Time{}
		return true, false, e.onSpeakingChange
	}
	return false, e.speaking, nil
}

// IsPushToTalk reports whether push-to-talk is currently held.
func (e *Engine) IsPushToTalk() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.pushToTalk
}

// IsEffectivelyMuted reports whether microphone audio is currently suppressed.
func (e *Engine) IsEffectivelyMuted() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isEffectivelyMutedLocked()
}

func (e *Engine) isEffectivelyMutedLocked() bool {
	if e.deafened {
		return true
	}
	if e.muted {
		return !e.pushToTalk
	}
	return false
}

// Speaking reports whether local voice activity is currently active.
func (e *Engine) Speaking() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.speaking
}

// PeerStats returns a copy of incoming audio statistics for peerID.
func (e *Engine) PeerStats(peerID string) (PeerAudioStats, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	stats, ok := e.peerStats[peerID]
	if !ok {
		return PeerAudioStats{}, false
	}
	copyStats := *stats
	if copyStats.Speaking && time.Since(copyStats.LastSpoke) > speakingHangover {
		copyStats.Speaking = false
	}
	return copyStats, true
}

// PeerSpeaking reports whether peerID recently transmitted audible speech.
func (e *Engine) PeerSpeaking(peerID string) bool {
	stats, ok := e.PeerStats(peerID)
	return ok && stats.Speaking
}

// Close shuts down the audio engine, closes platform streams, and stops frame pacing.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.running.Store(false)
		e.mu.Lock()
		e.closed = true
		e.speaking = false
		closePlatform := e.platformClose
		e.platformClose = nil
		e.platformPlay = nil
		e.mu.Unlock()

		e.tickerMu.Lock()
		if e.ticker != nil {
			e.ticker.Stop()
		}
		e.tickerMu.Unlock()

		close(e.closeCh)
		if closePlatform != nil {
			closePlatform()
		}
	})
	return nil
}

// ComputePCM16RMS computes the normalized RMS amplitude [0..1] of 16-bit little-endian PCM samples.
func ComputePCM16RMS(pcmLE []byte) float64 {
	n := len(pcmLE) &^ 1
	if n == 0 {
		return 0
	}
	var sumSq float64
	count := n / 2
	for i := 0; i < n; i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcmLE[i : i+2]))
		norm := float64(s) / 32768.0
		sumSq += norm * norm
	}
	return math.Sqrt(sumSq / float64(count))
}

// ScalePCM16 applies a volume percentage (0..200) to 16-bit little-endian PCM samples with clipping.
func ScalePCM16(pcmLE []byte, volume int) []byte {
	n := len(pcmLE) &^ 1
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	if volume == 100 {
		copy(out, pcmLE[:n])
		return out
	}
	if volume <= 0 {
		return out
	}
	for i := 0; i < n; i += 2 {
		s := int32(int16(binary.LittleEndian.Uint16(pcmLE[i : i+2])))
		scaled := (s * int32(volume)) / 100
		if scaled > math.MaxInt16 {
			scaled = math.MaxInt16
		} else if scaled < math.MinInt16 {
			scaled = math.MinInt16
		}
		binary.LittleEndian.PutUint16(out[i:i+2], uint16(int16(scaled)))
	}
	return out
}

// MixPCM sums multiple peers' 16-bit PCM frames using per-peer volume percentages (0..200)
// and clamps samples to [-32768, 32767].
func MixPCM(inputs map[string][]int16, volumes map[string]int, muted map[string]bool) []int16 {
	maxLen := 0
	for peerID, pcm := range inputs {
		if muted[peerID] || volumes[peerID] <= 0 {
			continue
		}
		if len(pcm) > maxLen {
			maxLen = len(pcm)
		}
	}
	if maxLen == 0 {
		return nil
	}

	accum := make([]int32, maxLen)
	for peerID, pcm := range inputs {
		if muted[peerID] {
			continue
		}
		vol := volumes[peerID]
		if vol <= 0 {
			continue
		}
		for i, s := range pcm {
			accum[i] += (int32(s) * int32(vol)) / 100
		}
	}

	out := make([]int16, maxLen)
	for i, v := range accum {
		if v > math.MaxInt16 {
			out[i] = math.MaxInt16
		} else if v < math.MinInt16 {
			out[i] = math.MinInt16
		} else {
			out[i] = int16(v)
		}
	}
	return out
}
