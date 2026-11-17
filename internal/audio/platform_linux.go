//go:build linux

package audio

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	linuxPCMBytesPerFrame = 640 // 16kHz * 1ch * 2 bytes * 20ms
	detectTimeout         = 1 * time.Second
)

// DetectAudioDevices queries PipeWire (wpctl) or PulseAudio (pactl) for the
// active default input microphone and output audio device descriptions.
func DetectAudioDevices() (mic string, output string) {
	mic = detectWPCTL("@DEFAULT_AUDIO_SOURCE@")
	if mic == "" {
		mic = detectPactl("source")
	}
	if mic == "" {
		mic = "Default Microphone"
	}

	output = detectWPCTL("@DEFAULT_AUDIO_SINK@")
	if output == "" {
		output = detectPactl("sink")
	}
	if output == "" {
		output = "Default Output"
	}
	return mic, output
}

func detectWPCTL(target string) string {
	if _, err := exec.LookPath("wpctl"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "wpctl", "inspect", target).Output()
	if err != nil {
		return ""
	}

	var nick, name string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "*"))
		if val, ok := parseWPCTLProp(line, "node.description"); ok {
			return val
		}
		if nick == "" {
			if val, ok := parseWPCTLProp(line, "node.nick"); ok {
				nick = val
			}
		}
		if name == "" {
			if val, ok := parseWPCTLProp(line, "node.name"); ok {
				name = val
			}
		}
	}
	if nick != "" {
		return nick
	}
	return name
}

func parseWPCTLProp(line, key string) (string, bool) {
	prefix := key + " ="
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	val := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	val = strings.Trim(val, `"`)
	if val == "" {
		return "", false
	}
	return val, true
}

func detectPactl(kind string) string {
	if _, err := exec.LookPath("pactl"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()

	defaultOut, err := exec.CommandContext(ctx, "pactl", "get-default-"+kind).Output()
	if err != nil {
		return ""
	}
	defaultName := strings.TrimSpace(string(defaultOut))
	if defaultName == "" {
		return ""
	}

	listOut, err := exec.CommandContext(ctx, "pactl", "list", kind+"s").Output()
	if err != nil {
		return defaultName
	}

	var currentName string
	scanner := bufio.NewScanner(bytes.NewReader(listOut))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Name:") {
			currentName = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			continue
		}
		if currentName == defaultName && strings.HasPrefix(line, "Description:") {
			desc := strings.TrimSpace(strings.TrimPrefix(line, "Description:"))
			if desc != "" {
				return desc
			}
		}
	}
	return defaultName
}

func hasPulseAudioServer() bool {
	if strings.TrimSpace(os.Getenv("PULSE_SERVER")) != "" {
		return true
	}
	if xdgRuntime := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); xdgRuntime != "" {
		if _, err := os.Stat(filepath.Join(xdgRuntime, "pulse", "native")); err == nil {
			return true
		}
	}
	uidPath := filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "pulse", "native")
	if _, err := os.Stat(uidPath); err == nil {
		return true
	}
	return false
}

func openPlatformStream(ctx context.Context) (func() ([]byte, float64), func([]byte), func()) {
	if !hasPulseAudioServer() {
		return nil, nil, func() {}
	}
	parecPath, errIn := exec.LookPath("parec")
	pacatPath, errOut := exec.LookPath("pacat")
	if errIn != nil || errOut != nil {
		return nil, nil, func() {}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	var (
		micMu     sync.Mutex
		latestMic []byte
		latestRMS float64
		hasUnread bool
	)

	inCmd := exec.CommandContext(streamCtx, parecPath,
		"--raw", "--format=s16le", "--rate=16000", "--channels=1", "--latency-msec=20",
	)
	inStdout, err := inCmd.StdoutPipe()
	if err == nil && inCmd.Start() == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, linuxPCMBytesPerFrame)
			for {
				if _, readErr := io.ReadFull(inStdout, buf); readErr != nil {
					return
				}
				rms := ComputePCM16RMS(buf)
				frameCopy := make([]byte, len(buf))
				copy(frameCopy, buf)

				micMu.Lock()
				latestMic = frameCopy
				latestRMS = rms
				hasUnread = true
				micMu.Unlock()
			}
		}()
	} else {
		inCmd = nil
	}

	outCmd := exec.CommandContext(streamCtx, pacatPath,
		"--raw", "--format=s16le", "--rate=16000", "--channels=1", "--latency-msec=20",
	)
	var playCh chan []byte
	outStdin, err := outCmd.StdinPipe()
	if err == nil && outCmd.Start() == nil {
		playCh = make(chan []byte, 16)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = outStdin.Close() }()
			for {
				select {
				case <-streamCtx.Done():
					return
				case frame, ok := <-playCh:
					if !ok {
						return
					}
					if _, writeErr := outStdin.Write(frame); writeErr != nil {
						return
					}
				}
			}
		}()
	} else {
		outCmd = nil
	}

	var micRead func() ([]byte, float64)
	if inCmd != nil {
		micRead = func() ([]byte, float64) {
			micMu.Lock()
			defer micMu.Unlock()
			if !hasUnread {
				return nil, 0
			}
			hasUnread = false
			return latestMic, latestRMS
		}
	}

	var playWrite func([]byte)
	if outCmd != nil && playCh != nil {
		playWrite = func(pcm []byte) {
			if len(pcm) == 0 {
				return
			}
			frame := make([]byte, len(pcm))
			copy(frame, pcm)
			select {
			case playCh <- frame:
			default:
			}
		}
	}

	var closeOnce sync.Once
	closeFn := func() {
		closeOnce.Do(func() {
			cancel()
			if inCmd != nil {
				_ = inCmd.Wait()
			}
			if outCmd != nil {
				_ = outCmd.Wait()
			}
			wg.Wait()
		})
	}

	return micRead, playWrite, closeFn
}
