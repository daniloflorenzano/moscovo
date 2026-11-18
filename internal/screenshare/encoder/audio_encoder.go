package encoder

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	// SystemAudioFrameDuration is the 20ms packetization interval for system audio tracks.
	SystemAudioFrameDuration = 20 * time.Millisecond
	// SystemAudioCaptureRate is the 48kHz stereo capture clock rate from desktop loopback.
	SystemAudioCaptureRate = 48000
	// SystemAudioCaptureChannels is stereo (2 channels).
	SystemAudioCaptureChannels = 2
	// SystemAudioFrameBytes48kStereo is 48000 * 2ch * 2B * 0.02s = 3840 bytes.
	SystemAudioFrameBytes48kStereo = 3840
	// SystemAudioFrameBytes16kMono is 16000 * 1ch * 2B * 0.02s = 640 bytes.
	SystemAudioFrameBytes16kMono = 640
)

var opusDTXSilenceFrame = []byte{0xF8, 0xFF, 0xFE}

// SystemAudioEncoder packages desktop loopback PCM frames for the isolated screenshare-audio track
// without passing through microphone voice processing or noise suppression (FR-06).
type SystemAudioEncoder struct {
	mu           sync.Mutex
	sampleRate   int
	channels     int
	silenceCount uint64
	closed       bool
}

// NewSystemAudioEncoder creates a system-audio packetizer expecting loopback PCM16 frames.
func NewSystemAudioEncoder(sampleRate, channels int) *SystemAudioEncoder {
	if sampleRate <= 0 {
		sampleRate = SystemAudioCaptureRate
	}
	if channels <= 0 {
		channels = SystemAudioCaptureChannels
	}
	return &SystemAudioEncoder{
		sampleRate: sampleRate,
		channels:   channels,
	}
}

// EncodeFrame converts a 20ms loopback s16le PCM buffer into a track sample.
// When the buffer is silent (all samples below digital floor), it emits a compact DTX frame.
func (e *SystemAudioEncoder) EncodeFrame(pcmLE []byte) (media.Sample, bool) {
	e.mu.Lock()
	if e.closed || len(pcmLE) < 4 {
		e.mu.Unlock()
		return media.Sample{}, false
	}
	sr := e.sampleRate
	ch := e.channels
	e.mu.Unlock()

	downsampled := pcmLE
	if sr == 48000 && ch == 2 {
		downsampled = Downsample48kStereoTo16kMono(pcmLE)
	} else if sr == 48000 && ch == 1 {
		downsampled = Downsample48kMonoTo16kMono(pcmLE)
	}

	if isDigitalSilencePCM16(downsampled) {
		e.mu.Lock()
		e.silenceCount++
		emitSilence := e.silenceCount == 1 || e.silenceCount%25 == 0
		e.mu.Unlock()
		if !emitSilence {
			return media.Sample{}, false
		}
		return media.Sample{
			Data:     append([]byte(nil), opusDTXSilenceFrame...),
			Duration: SystemAudioFrameDuration,
		}, true
	}

	e.mu.Lock()
	e.silenceCount = 0
	e.mu.Unlock()

	copied := make([]byte, len(downsampled))
	copy(copied, downsampled)
	return media.Sample{
		Data:     copied,
		Duration: SystemAudioFrameDuration,
	}, true
}

// Close marks the encoder closed.
func (e *SystemAudioEncoder) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return nil
}

// Downsample48kStereoTo16kMono decimates 48kHz stereo s16le PCM (3x, 2ch->1ch) to 16kHz mono s16le.
func Downsample48kStereoTo16kMono(pcm48kStereo []byte) []byte {
	const inStep = 12 // 3 stereo frames * 4 bytes per stereo frame
	nBlocks := len(pcm48kStereo) / inStep
	if nBlocks == 0 {
		return nil
	}
	out := make([]byte, nBlocks*2)
	for i := 0; i < nBlocks; i++ {
		off := i * inStep
		var sum int32
		for s := 0; s < 3; s++ {
			p := off + s*4
			l := int32(int16(binary.LittleEndian.Uint16(pcm48kStereo[p : p+2])))
			r := int32(int16(binary.LittleEndian.Uint16(pcm48kStereo[p+2 : p+4])))
			sum += (l + r) >> 1
		}
		avg := int16(sum / 3)
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(avg))
	}
	return out
}

// Downsample48kMonoTo16kMono decimates 48kHz mono s16le PCM to 16kHz mono s16le.
func Downsample48kMonoTo16kMono(pcm48kMono []byte) []byte {
	const inStep = 6 // 3 mono frames * 2 bytes
	nBlocks := len(pcm48kMono) / inStep
	if nBlocks == 0 {
		return nil
	}
	out := make([]byte, nBlocks*2)
	for i := 0; i < nBlocks; i++ {
		off := i * inStep
		var sum int32
		for s := 0; s < 3; s++ {
			p := off + s*2
			sum += int32(int16(binary.LittleEndian.Uint16(pcm48kMono[p : p+2])))
		}
		avg := int16(sum / 3)
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(avg))
	}
	return out
}

func isDigitalSilencePCM16(pcmLE []byte) bool {
	n := len(pcmLE) &^ 1
	for i := 0; i < n; i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcmLE[i : i+2]))
		if v > 8 || v < -8 {
			return false
		}
	}
	return true
}
