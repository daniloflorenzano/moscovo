//go:build !linux && !windows

package audio

import "context"

// DetectAudioDevices returns fallback device names on unsupported platforms.
func DetectAudioDevices() (mic string, output string) {
	return "Default Microphone", "Default Output"
}

func openPlatformStream(context.Context) (func() ([]byte, float64), func([]byte), func()) {
	return nil, nil, func() {}
}
