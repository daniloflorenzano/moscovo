//go:build windows

package audio

import (
	"context"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	winPCMBytesPerFrame = 640 // 16kHz * 1ch * 2 bytes * 20ms
	winRingBuffers      = 4
	waveFormatPCM       = 1
	waveMapper          = uintptr(0xFFFFFFFF)
	whdrDone            = 0x00000001
	whdrPrepared        = 0x00000002
)

var (
	winmm = syscall.NewLazyDLL("winmm.dll")

	procWaveInGetNumDevs      = winmm.NewProc("waveInGetNumDevs")
	procWaveInGetDevCapsW     = winmm.NewProc("waveInGetDevCapsW")
	procWaveOutGetNumDevs     = winmm.NewProc("waveOutGetNumDevs")
	procWaveOutGetDevCapsW    = winmm.NewProc("waveOutGetDevCapsW")
	procWaveInOpen            = winmm.NewProc("waveInOpen")
	procWaveInPrepareHeader   = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer       = winmm.NewProc("waveInAddBuffer")
	procWaveInStart           = winmm.NewProc("waveInStart")
	procWaveInReset           = winmm.NewProc("waveInReset")
	procWaveInClose           = winmm.NewProc("waveInClose")
	procWaveOutOpen           = winmm.NewProc("waveOutOpen")
	procWaveOutPrepareHeader  = winmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepareHdr   = winmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite          = winmm.NewProc("waveOutWrite")
	procWaveOutReset          = winmm.NewProc("waveOutReset")
	procWaveOutClose          = winmm.NewProc("waveOutClose")
)

type waveInCapsW struct {
	Mid           uint16
	Pid           uint16
	DriverVersion uint32
	Pname         [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved1     uint16
}

type waveOutCapsW struct {
	Mid           uint16
	Pid           uint16
	DriverVersion uint32
	Pname         [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved1     uint16
	Support       uint32
}

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

// DetectAudioDevices queries winmm.dll for the default Windows capture and playback devices.
func DetectAudioDevices() (mic string, output string) {
	mic = "Default Microphone"
	output = "Default Output"

	if n, _, _ := procWaveInGetNumDevs.Call(); n > 0 {
		var caps waveInCapsW
		r, _, _ := procWaveInGetDevCapsW.Call(0, uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
		if r == 0 {
			if name := syscall.UTF16ToString(caps.Pname[:]); name != "" {
				mic = name
			}
		}
	}

	if n, _, _ := procWaveOutGetNumDevs.Call(); n > 0 {
		var caps waveOutCapsW
		r, _, _ := procWaveOutGetDevCapsW.Call(0, uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
		if r == 0 {
			if name := syscall.UTF16ToString(caps.Pname[:]); name != "" {
				output = name
			}
		}
	}
	return mic, output
}

func openPlatformStream(ctx context.Context) (func() ([]byte, float64), func([]byte), func()) {
	wfx := waveFormatEx{
		FormatTag:      waveFormatPCM,
		Channels:       1,
		SamplesPerSec:  16000,
		AvgBytesPerSec: 32000,
		BlockAlign:     2,
		BitsPerSample:  16,
		Size:           0,
	}

	var pinner runtime.Pinner
	var wg sync.WaitGroup
	stopCh := make(chan struct{})

	var hWaveIn uintptr
	rIn, _, _ := procWaveInOpen.Call(
		uintptr(unsafe.Pointer(&hWaveIn)),
		waveMapper,
		uintptr(unsafe.Pointer(&wfx)),
		0, 0, 0,
	)

	var (
		micMu     sync.Mutex
		latestMic []byte
		latestRMS float64
		hasUnread bool
		inHdrs    []waveHdr
		inBufs    [][]byte
	)

	if rIn == 0 && hWaveIn != 0 {
		inHdrs = make([]waveHdr, winRingBuffers)
		inBufs = make([][]byte, winRingBuffers)
		pinner.Pin(&inHdrs[0])

		for i := range inHdrs {
			inBufs[i] = make([]byte, winPCMBytesPerFrame)
			pinner.Pin(&inBufs[i][0])
			inHdrs[i].Data = uintptr(unsafe.Pointer(&inBufs[i][0]))
			inHdrs[i].BufferLength = uint32(len(inBufs[i]))
			procWaveInPrepareHeader.Call(hWaveIn, uintptr(unsafe.Pointer(&inHdrs[i])), unsafe.Sizeof(inHdrs[i]))
			procWaveInAddBuffer.Call(hWaveIn, uintptr(unsafe.Pointer(&inHdrs[i])), unsafe.Sizeof(inHdrs[i]))
		}
		procWaveInStart.Call(hWaveIn)

		wg.Add(1)
		go func() {
			defer wg.Done()
			idx := 0
			for {
				select {
				case <-stopCh:
					return
				case <-ctx.Done():
					return
				default:
				}
				hdr := &inHdrs[idx]
				if hdr.Flags&whdrDone != 0 {
					recorded := int(hdr.BytesRecorded)
					if recorded > 0 && recorded <= len(inBufs[idx]) {
						frame := make([]byte, recorded)
						copy(frame, inBufs[idx][:recorded])
						rms := ComputePCM16RMS(frame)

						micMu.Lock()
						latestMic = frame
						latestRMS = rms
						hasUnread = true
						micMu.Unlock()
					}
					hdr.Flags &^= whdrDone
					hdr.BytesRecorded = 0
					procWaveInAddBuffer.Call(hWaveIn, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
					idx = (idx + 1) % len(inHdrs)
				} else {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	} else {
		hWaveIn = 0
	}

	var hWaveOut uintptr
	rOut, _, _ := procWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&hWaveOut)),
		waveMapper,
		uintptr(unsafe.Pointer(&wfx)),
		0, 0, 0,
	)

	var (
		outMu   sync.Mutex
		outHdrs []waveHdr
		outBufs [][]byte
		outIdx  int
	)
	if rOut == 0 && hWaveOut != 0 {
		outHdrs = make([]waveHdr, winRingBuffers)
		outBufs = make([][]byte, winRingBuffers)
		pinner.Pin(&outHdrs[0])

		for i := range outHdrs {
			outBufs[i] = make([]byte, winPCMBytesPerFrame)
			pinner.Pin(&outBufs[i][0])
			outHdrs[i].Data = uintptr(unsafe.Pointer(&outBufs[i][0]))
			outHdrs[i].BufferLength = uint32(len(outBufs[i]))
			outHdrs[i].Flags = whdrDone
		}
	} else {
		hWaveOut = 0
	}

	var micRead func() ([]byte, float64)
	if hWaveIn != 0 {
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
	if hWaveOut != 0 {
		playWrite = func(pcm []byte) {
			if len(pcm) == 0 {
				return
			}
			outMu.Lock()
			defer outMu.Unlock()
			select {
			case <-stopCh:
				return
			default:
			}
			hdr := &outHdrs[outIdx]
			if hdr.Flags&whdrDone == 0 {
				return
			}
			if hdr.Flags&whdrPrepared != 0 {
				procWaveOutUnprepareHdr.Call(hWaveOut, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
			}
			if cap(outBufs[outIdx]) < len(pcm) {
				outBufs[outIdx] = make([]byte, len(pcm))
				pinner.Pin(&outBufs[outIdx][0])
				hdr.Data = uintptr(unsafe.Pointer(&outBufs[outIdx][0]))
			}
			outBufs[outIdx] = outBufs[outIdx][:len(pcm)]
			copy(outBufs[outIdx], pcm)
			hdr.BufferLength = uint32(len(pcm))
			hdr.Flags = 0
			procWaveOutPrepareHeader.Call(hWaveOut, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
			procWaveOutWrite.Call(hWaveOut, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
			outIdx = (outIdx + 1) % len(outHdrs)
		}
	}

	var closeOnce sync.Once
	closeFn := func() {
		closeOnce.Do(func() {
			close(stopCh)
			wg.Wait()

			if hWaveIn != 0 {
				procWaveInReset.Call(hWaveIn)
				for i := range inHdrs {
					procWaveInUnprepareHeader.Call(hWaveIn, uintptr(unsafe.Pointer(&inHdrs[i])), unsafe.Sizeof(inHdrs[i]))
				}
				procWaveInClose.Call(hWaveIn)
			}
			if hWaveOut != 0 {
				outMu.Lock()
				procWaveOutReset.Call(hWaveOut)
				for i := range outHdrs {
					if outHdrs[i].Flags&whdrPrepared != 0 {
						procWaveOutUnprepareHdr.Call(hWaveOut, uintptr(unsafe.Pointer(&outHdrs[i])), unsafe.Sizeof(outHdrs[i]))
					}
				}
				procWaveOutClose.Call(hWaveOut)
				outMu.Unlock()
			}
			pinner.Unpin()
		})
	}

	return micRead, playWrite, closeFn
}
