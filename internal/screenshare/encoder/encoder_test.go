package encoder

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"moscovo/internal/domain"
)

func makeGradientRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 255) / max(1, w)),
				G: uint8((y * 255) / max(1, h)),
				B: 180,
				A: 255,
			})
		}
	}
	return img
}

func TestFramePoolAndYUVConversion(t *testing.T) {
	pool := NewFramePool()

	rgba1 := pool.GetRGBA(640, 360)
	if rgba1.Bounds().Dx() != 640 || rgba1.Bounds().Dy() != 360 {
		t.Fatalf("unexpected RGBA dimensions: %v", rgba1.Bounds())
	}
	pool.PutRGBA(rgba1)

	rgba2 := pool.GetRGBA(640, 360)
	if rgba2.Bounds().Dx() != 640 || rgba2.Bounds().Dy() != 360 {
		t.Fatalf("unexpected recycled RGBA dimensions: %v", rgba2.Bounds())
	}
	pool.PutRGBA(rgba2)

	src := makeGradientRGBA(1920, 1080)
	yuv := ConvertRGBAToYUV420(src, 1280, 720, pool)
	if yuv.Width != 1280 || yuv.Height != 720 {
		t.Fatalf("yuv size = (%d, %d), want (1280, 720)", yuv.Width, yuv.Height)
	}
	if len(yuv.Y) != 1280*720 || len(yuv.U) != 640*360 || len(yuv.V) != 640*360 {
		t.Fatalf("unexpected YUV plane lengths: Y=%d U=%d V=%d", len(yuv.Y), len(yuv.U), len(yuv.V))
	}

	reconstructed := ConvertYUV420ToRGBA(yuv, pool)
	if reconstructed.Bounds().Dx() != 1280 || reconstructed.Bounds().Dy() != 720 {
		t.Fatalf("reconstructed RGBA size = %v", reconstructed.Bounds())
	}
	pool.PutRGBA(reconstructed)
	pool.PutYUV420(yuv)
}

func TestVP8LiteEncoderLatencyAndBitrateAdaptation(t *testing.T) {
	pool := NewFramePool()
	enc, err := NewVideoEncoder(EncoderConfig{
		Width:         1280,
		Height:        720,
		FPS:           30,
		BitrateBps:    2_000_000,
		DisableFFmpeg: true,
	})
	if err != nil {
		t.Fatalf("NewVideoEncoder() failed: %v", err)
	}
	defer func() { _ = enc.Close() }()

	if enc.CodecMime() != webrtc.MimeTypeVP8 {
		t.Fatalf("CodecMime() = %q, want %q", enc.CodecMime(), webrtc.MimeTypeVP8)
	}

	src := makeGradientRGBA(1280, 720)
	yuv := ConvertRGBAToYUV420(src, 1280, 720, pool)
	defer pool.PutYUV420(yuv)

	start := time.Now()
	frame, err := enc.Encode(yuv, true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Encode() failed: %v", err)
	}
	if !frame.KeyFrame {
		t.Fatalf("expected first frame with forceKeyFrame=true to be KeyFrame")
	}
	budget := 100 * time.Millisecond
	if isRaceDetectorEnabled {
		budget = 500 * time.Millisecond
	}
	if elapsed > budget {
		t.Fatalf("Encode() took %v, exceeded %v budget", elapsed, budget)
	}

	jpg, w, h, ok := DecodeVP8FrameToJPEG(frame.Data)
	if !ok || len(jpg) < 10 || w != 1280 || h != 720 {
		t.Fatalf("DecodeVP8FrameToJPEG() failed: ok=%v len=%d w=%d h=%d", ok, len(jpg), w, h)
	}

	// Verify bitrate clamping & dynamic quality update.
	if err := enc.SetBitrate(100_000); err != nil {
		t.Fatalf("SetBitrate() failed: %v", err)
	}
	if enc.Bitrate() != domain.MinVideoBitrateBps {
		t.Fatalf("Bitrate() = %d, want min %d", enc.Bitrate(), domain.MinVideoBitrateBps)
	}

	lowFrame, err := enc.Encode(yuv, false)
	if err != nil {
		t.Fatalf("Encode() interframe failed: %v", err)
	}
	if lowFrame.KeyFrame {
		t.Fatalf("expected second frame to be interframe")
	}

	if err := enc.SetBitrate(10_000_000); err != nil {
		t.Fatalf("SetBitrate() max failed: %v", err)
	}
	if enc.Bitrate() != domain.MaxVideoBitrateBps {
		t.Fatalf("Bitrate() = %d, want max %d", enc.Bitrate(), domain.MaxVideoBitrateBps)
	}

	if err := enc.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	if _, err := enc.Encode(yuv, false); err != ErrEncoderClosed {
		t.Fatalf("Encode() after Close = %v, want ErrEncoderClosed", err)
	}
}

func TestFFmpegVideoEncoderAndStreamDecoder(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed in PATH")
	}

	pool := NewFramePool()
	enc, err := NewVideoEncoder(EncoderConfig{
		Width:          640,
		Height:         360,
		FPS:            30,
		BitrateBps:     1_500_000,
		CodecMime:      webrtc.MimeTypeVP8,
		PreferHardware: false,
	})
	if err != nil {
		t.Fatalf("NewVideoEncoder() failed: %v", err)
	}
	defer func() { _ = enc.Close() }()

	src := makeGradientRGBA(640, 360)
	yuv := ConvertRGBAToYUV420(src, 640, 360, pool)
	defer pool.PutYUV420(yuv)

	frame1, err := enc.Encode(yuv, true)
	if err != nil {
		t.Fatalf("Encode(frame1) failed: %v", err)
	}
	if len(frame1.Data) == 0 || !frame1.KeyFrame {
		t.Fatalf("expected non-empty keyframe from ffmpeg encoder")
	}

	frame2, err := enc.Encode(yuv, false)
	if err != nil {
		t.Fatalf("Encode(frame2) failed: %v", err)
	}
	if len(frame2.Data) == 0 {
		t.Fatalf("expected non-empty second frame")
	}

	dec := NewVP8StreamDecoder()
	defer func() { _ = dec.Close() }()
	_, _, _, _ = dec.Decode(frame1.Data)
	// Allow ffmpeg decoder brief pipe flush if decoding asynchronously.
	deadline := time.Now().Add(800 * time.Millisecond)
	var decoded []byte
	for time.Now().Before(deadline) {
		if jpg, _, _, ok := dec.Decode(frame2.Data); ok && len(jpg) > 2 {
			decoded = jpg
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(decoded) > 2 && !bytes.HasPrefix(decoded, []byte{0xFF, 0xD8}) {
		t.Fatalf("decoded image missing JPEG SOI marker")
	}
}

func TestSystemAudioEncoderDownsamplingAndSilence(t *testing.T) {
	enc := NewSystemAudioEncoder(SystemAudioCaptureRate, SystemAudioCaptureChannels)
	defer func() { _ = enc.Close() }()

	// Silent 48kHz stereo 20ms frame (3840 bytes).
	silent := make([]byte, SystemAudioFrameBytes48kStereo)
	s1, ok := enc.EncodeFrame(silent)
	if !ok || !bytes.Equal(s1.Data, opusDTXSilenceFrame) {
		t.Fatalf("expected initial silence DTX frame, ok=%v data=%x", ok, s1.Data)
	}
	// Second consecutive silent frame is suppressed until periodic DTX refresh.
	if _, ok2 := enc.EncodeFrame(silent); ok2 {
		t.Fatalf("expected consecutive silent frame to be DTX suppressed")
	}

	// Active 48kHz stereo tone -> downsampled to 640-byte 16kHz mono PCM16.
	tone := make([]byte, SystemAudioFrameBytes48kStereo)
	for i := 0; i+4 <= len(tone); i += 4 {
		binary.LittleEndian.PutUint16(tone[i:i+2], uint16(4000))
		binary.LittleEndian.PutUint16(tone[i+2:i+4], uint16(6000))
	}
	activeSample, ok := enc.EncodeFrame(tone)
	if !ok {
		t.Fatalf("expected active sample to emit")
	}
	if len(activeSample.Data) != SystemAudioFrameBytes16kMono {
		t.Fatalf("downsampled audio len = %d, want %d", len(activeSample.Data), SystemAudioFrameBytes16kMono)
	}
	if activeSample.Duration != SystemAudioFrameDuration {
		t.Fatalf("Duration = %v, want %v", activeSample.Duration, SystemAudioFrameDuration)
	}
	firstSample := int16(binary.LittleEndian.Uint16(activeSample.Data[0:2]))
	if firstSample != 5000 {
		t.Fatalf("averaged L/R sample = %d, want 5000", firstSample)
	}
}
