package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func makePCM16Frame(sampleVal int16, samples int) []byte {
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:i*2+2], uint16(sampleVal))
	}
	return buf
}

func TestEngineProcessedMicFrameAndFallback(t *testing.T) {
	rawFrame := makePCM16Frame(3000, 320) // raw RMS ~0.0915 (above speakingThreshold)
	rawRMS := ComputePCM16RMS(rawFrame)
	quietProcessed := makePCM16Frame(10, 320) // suppressed noise RMS ~0.0003 (below speakingThreshold)

	var (
		mu       sync.Mutex
		rawSeen  [][]byte
	)

	eng := NewEngine(EngineConfig{
		FrameDuration: 15 * time.Millisecond,
		MicSource: func() ([]byte, float64) {
			return rawFrame, rawRMS
		},
		OnRawMicFrame: func(pcm []byte) {
			mu.Lock()
			rawSeen = append(rawSeen, append([]byte(nil), pcm...))
			mu.Unlock()
		},
	})
	defer func() { _ = eng.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Push a noise-suppressed quiet frame before reading; ReadMicSample should emit it
	// and suppress speaking detection while still forwarding the raw mic frame to OnRawMicFrame.
	eng.PushProcessedMicFrame(quietProcessed)
	sample, err := eng.ReadMicSample(ctx)
	if err != nil {
		t.Fatalf("ReadMicSample() failed: %v", err)
	}
	if !bytes.Equal(sample.Data, quietProcessed) {
		t.Fatalf("expected processed mic frame, got len=%d", len(sample.Data))
	}
	if eng.Speaking() {
		t.Fatalf("expected Speaking() == false for quiet processed frame")
	}

	mu.Lock()
	seenCount := len(rawSeen)
	var firstRaw []byte
	if seenCount > 0 {
		firstRaw = rawSeen[0]
	}
	mu.Unlock()
	if seenCount != 1 || !bytes.Equal(firstRaw, rawFrame) {
		t.Fatalf("expected OnRawMicFrame to receive 1 raw frame matching MicSource")
	}

	// 2. Without another PushProcessedMicFrame, processedMic was consumed, so ReadMicSample
	// falls back seamlessly to raw MicSource and triggers speaking detection.
	sample2, err := eng.ReadMicSample(ctx)
	if err != nil {
		t.Fatalf("ReadMicSample() second tick failed: %v", err)
	}
	if !bytes.Equal(sample2.Data, rawFrame) {
		t.Fatalf("expected fallback to raw mic frame")
	}
	if !eng.Speaking() {
		t.Fatalf("expected Speaking() == true on raw fallback frame")
	}

	// 3. ClearProcessedMicFrame discards a queued processed frame immediately.
	eng.PushProcessedMicFrame(quietProcessed)
	eng.ClearProcessedMicFrame()
	sample3, err := eng.ReadMicSample(ctx)
	if err != nil {
		t.Fatalf("ReadMicSample() third tick failed: %v", err)
	}
	if !bytes.Equal(sample3.Data, rawFrame) {
		t.Fatalf("expected raw mic frame after ClearProcessedMicFrame")
	}

	// 4. Stale processed frame (> 150ms old) is ignored in favor of raw MicSource.
	eng.mu.Lock()
	eng.processedMic = append([]byte(nil), quietProcessed...)
	eng.processedRMS = ComputePCM16RMS(quietProcessed)
	eng.processedAt = time.Now().Add(-200 * time.Millisecond)
	eng.mu.Unlock()

	sample4, err := eng.ReadMicSample(ctx)
	if err != nil {
		t.Fatalf("ReadMicSample() fourth tick failed: %v", err)
	}
	if !bytes.Equal(sample4.Data, rawFrame) {
		t.Fatalf("expected raw mic frame when processed frame is stale")
	}
}
