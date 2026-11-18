package capture

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

// PipelineConfig wires a platform Capturer to video/audio encoders and WebRTC callbacks.
type PipelineConfig struct {
	Capturer      Capturer
	Encoder       encoder.VideoEncoder
	AudioEncoder  *encoder.SystemAudioEncoder
	Pool          *encoder.FramePool
	TargetWidth   int
	TargetHeight  int
	OnVideoSample func(encoder.EncodedFrame)
	OnAudioSample func(data []byte, duration time.Duration)
	OnStopped     func(reason error)
}

// Pipeline runs low-latency drop-frame video encoding and isolated system-audio packetization.
type Pipeline struct {
	capturer      Capturer
	encoder       encoder.VideoEncoder
	audioEncoder  *encoder.SystemAudioEncoder
	pool          *encoder.FramePool
	targetW       int
	targetH       int
	onVideoSample func(encoder.EncodedFrame)
	onAudioSample func([]byte, time.Duration)
	onStopped     func(error)

	forceKeyframe atomic.Bool
	droppedFrames atomic.Uint64
	encodedFrames atomic.Uint64

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  atomic.Bool
}

// NewPipeline initializes a screen-share capture/encode pipeline.
func NewPipeline(cfg PipelineConfig) (*Pipeline, error) {
	if cfg.Capturer == nil {
		return nil, errors.New("capturer cannot be nil")
	}
	if cfg.Encoder == nil {
		return nil, errors.New("video encoder cannot be nil")
	}
	pool := cfg.Pool
	if pool == nil {
		pool = encoder.NewFramePool()
	}
	return &Pipeline{
		capturer:      cfg.Capturer,
		encoder:       cfg.Encoder,
		audioEncoder:  cfg.AudioEncoder,
		pool:          pool,
		targetW:       cfg.TargetWidth,
		targetH:       cfg.TargetHeight,
		onVideoSample: cfg.OnVideoSample,
		onAudioSample: cfg.OnAudioSample,
		onStopped:     cfg.OnStopped,
	}, nil
}

// Start begins capturing and encoding frames until ctx is cancelled, Stop is called, or the source closes.
func (p *Pipeline) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	if err := p.capturer.Start(runCtx); err != nil {
		cancel()
		return err
	}

	slot := make(chan FramePacket, 1)

	// Producer: pull captured frames and drop stale buffered frames under backpressure (NFR-04).
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(slot)

		for {
			select {
			case <-runCtx.Done():
				return
			case pkt, ok := <-p.capturer.Frames():
				if !ok {
					return
				}
				select {
				case stale := <-slot:
					if stale.Release != nil {
						stale.Release()
					}
					p.droppedFrames.Add(1)
				default:
				}
				select {
				case slot <- pkt:
				default:
					if pkt.Release != nil {
						pkt.Release()
					}
					p.droppedFrames.Add(1)
				}
			}
		}
	}()

	// Consumer: convert RGBA -> YUV420p using FramePool, encode, and emit sample.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.notifyStoppedOnExit(runCtx)

		for pkt := range slot {
			if runCtx.Err() != nil {
				if pkt.Release != nil {
					pkt.Release()
				}
				continue
			}

			dstW, dstH := p.targetW, p.targetH
			if pkt.Image != nil {
				if dstW <= 0 || dstH <= 0 {
					dstW = pkt.Image.Rect.Dx()
					dstH = pkt.Image.Rect.Dy()
				}
			}
			yuv := encoder.ConvertRGBAToYUV420(pkt.Image, dstW, dstH, p.pool)
			if pkt.Release != nil {
				pkt.Release()
			}

			forceKF := p.forceKeyframe.Swap(false)
			encoded, err := p.encoder.Encode(yuv, forceKF)
			p.pool.PutYUV420(yuv)
			if err != nil {
				if errors.Is(err, encoder.ErrEncoderClosed) || runCtx.Err() != nil {
					continue
				}
				continue
			}

			p.encodedFrames.Add(1)
			if p.onVideoSample != nil {
				p.onVideoSample(encoded)
			}
		}
	}()

	// Isolated system-audio loopback pump (FR-06).
	if audioCh := p.capturer.AudioFrames(); audioCh != nil && p.audioEncoder != nil {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case <-runCtx.Done():
					return
				case pcm, ok := <-audioCh:
					if !ok {
						return
					}
					sample, emit := p.audioEncoder.EncodeFrame(pcm)
					if emit && p.onAudioSample != nil {
						p.onAudioSample(sample.Data, sample.Duration)
					}
				}
			}
		}()
	}

	return nil
}

func (p *Pipeline) notifyStoppedOnExit(runCtx context.Context) {
	// Only fire automatic OnStopped when capture terminated unexpectedly (source closed/revoked),
	// or if Stop() wasn't explicitly invoked yet.
	if p.stopped.Load() || runCtx.Err() != nil {
		return
	}
	reason := p.capturer.Err()
	if reason == nil {
		reason = ErrSourceClosed
	}
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		if p.cancel != nil {
			p.cancel()
		}
		_ = p.capturer.Close()
		_ = p.encoder.Close()
		if p.audioEncoder != nil {
			_ = p.audioEncoder.Close()
		}
		if p.onStopped != nil {
			go p.onStopped(reason)
		}
	})
}

// RequestKeyFrame schedules an immediate keyframe on the next encoded frame (e.g. RTCP PLI/FIR).
func (p *Pipeline) RequestKeyFrame() {
	p.forceKeyframe.Store(true)
}

// ApplyFeedback updates encoder target bitrate dynamically using RTCP REMB/loss/RTT metrics (NFR-04).
func (p *Pipeline) ApplyFeedback(rembBps uint64, lossFraction float64, rttMs int) int {
	current := p.encoder.Bitrate()
	next := domain.ComputeAdaptiveBitrate(current, rembBps, lossFraction, rttMs)
	if next != current {
		_ = p.encoder.SetBitrate(next)
	}
	return next
}

// Stats returns counts of encoded and dropped video frames.
func (p *Pipeline) Stats() (encoded uint64, dropped uint64) {
	return p.encodedFrames.Load(), p.droppedFrames.Load()
}

// Stop tears down the capture and encode pipeline cleanly and releases all resources.
func (p *Pipeline) Stop() error {
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		if p.cancel != nil {
			p.cancel()
		}
		_ = p.capturer.Close()
		_ = p.encoder.Close()
		if p.audioEncoder != nil {
			_ = p.audioEncoder.Close()
		}
	})
	p.wg.Wait()
	return nil
}
