package encoder

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os/exec"
	"sync"
	"time"
)

// VP8StreamDecoder extracts JPEG preview frames from incoming VP8 video samples
// for WebViews that render remote screens onto an HTML5 canvas fallback.
type VP8StreamDecoder struct {
	mu           sync.Mutex
	closed       bool
	ffmpegPath   string
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	width        int
	height       int
	frameCh      chan []byte
	lastDecodeAt time.Time
	done         chan struct{}
}

// NewVP8StreamDecoder constructs a decoder that handles both vp8Lite frames and real VP8 streams.
func NewVP8StreamDecoder() *VP8StreamDecoder {
	ffmpegPath, _ := exec.LookPath("ffmpeg")
	return &VP8StreamDecoder{
		ffmpegPath: ffmpegPath,
		frameCh:    make(chan []byte, 1),
		done:       make(chan struct{}),
	}
}

// Decode attempts to convert an incoming VP8 payload into JPEG bytes.
// For vp8Lite frames it decodes inline; for real VP8 bitstreams it uses a persistent ffmpeg pipe if available.
func (d *VP8StreamDecoder) Decode(payload []byte) (jpegData []byte, width int, height int, ok bool) {
	if len(payload) == 0 {
		return nil, 0, 0, false
	}
	if jpg, w, h, lite := DecodeVP8FrameToJPEG(payload); lite {
		return jpg, w, h, true
	}

	d.mu.Lock()
	if d.closed || d.ffmpegPath == "" {
		d.mu.Unlock()
		return nil, 0, 0, false
	}

	// Parse keyframe dimensions from RFC 6386 VP8 header (0x9d 0x01 0x2a).
	if (payload[0]&0x01) == 0 && len(payload) >= 10 &&
		payload[3] == 0x9d && payload[4] == 0x01 && payload[5] == 0x2a {
		w := int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
		if w > 0 && h > 0 && (d.cmd == nil || w != d.width || h != d.height) {
			d.stopPipeLocked()
			_ = d.startPipeLocked(w, h)
		}
	}

	if d.stdin != nil && d.width > 0 && d.height > 0 {
		var fhdr [12]byte
		binary.LittleEndian.PutUint32(fhdr[0:4], uint32(len(payload)))
		if _, err := d.stdin.Write(fhdr[:]); err == nil {
			_, _ = d.stdin.Write(payload)
		}
	}
	w, h := d.width, d.height
	d.mu.Unlock()

	select {
	case jpg := <-d.frameCh:
		return jpg, w, h, len(jpg) > 0
	default:
		return nil, 0, 0, false
	}
}

func (d *VP8StreamDecoder) startPipeLocked(w, h int) error {
	cmd := exec.Command(d.ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "ivf",
		"-i", "pipe:0",
		"-vf", "format=yuvj420p",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "8",
		"-flush_packets", "1",
		"pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}

	var ivfHdr [32]byte
	copy(ivfHdr[0:4], "DKIF")
	binary.LittleEndian.PutUint16(ivfHdr[4:6], 0)
	binary.LittleEndian.PutUint16(ivfHdr[6:8], 32)
	copy(ivfHdr[8:12], "VP80")
	binary.LittleEndian.PutUint16(ivfHdr[12:14], uint16(w))
	binary.LittleEndian.PutUint16(ivfHdr[14:16], uint16(h))
	binary.LittleEndian.PutUint32(ivfHdr[16:20], 30)
	binary.LittleEndian.PutUint32(ivfHdr[20:24], 1)
	if _, err := stdin.Write(ivfHdr[:]); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}

	d.cmd = cmd
	d.stdin = stdin
	d.width = w
	d.height = h
	go d.readJPEGFrames(stdout)
	return nil
}

func (d *VP8StreamDecoder) readJPEGFrames(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, 64*1024)
	var frame bytes.Buffer
	inFrame := false
	var prev byte

	for {
		b, err := reader.ReadByte()
		if err != nil {
			return
		}
		if !inFrame {
			if prev == 0xFF && b == 0xD8 {
				frame.Reset()
				frame.WriteByte(0xFF)
				frame.WriteByte(0xD8)
				inFrame = true
				prev = 0
				continue
			}
			prev = b
			continue
		}

		frame.WriteByte(b)
		if prev == 0xFF && b == 0xD9 {
			inFrame = false
			jpg := append([]byte(nil), frame.Bytes()...)
			// Keep only the latest decoded frame.
			select {
			case <-d.frameCh:
			default:
			}
			select {
			case d.frameCh <- jpg:
			default:
			}
			prev = 0
			continue
		}
		prev = b
	}
}

func (d *VP8StreamDecoder) stopPipeLocked() {
	if d.stdin != nil {
		_ = d.stdin.Close()
		d.stdin = nil
	}
	if d.cmd != nil {
		cmd := d.cmd
		d.cmd = nil
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	}
}

// Close releases any underlying decoder subprocess cleanly.
func (d *VP8StreamDecoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.stopPipeLocked()
	return nil
}
