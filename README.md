# Moscovo

Minimalist peer-to-peer (P2P) desktop voice communication system built with **Go**, **Wails v3**, and **Pion WebRTC**, designed for low-latency voice calls across different Tailnets via a **Shared Tailscale Hub**.

## How It Works

Because Tailscale Node Sharing is non-transitive (peers on different Tailnets can reach a shared server, but cannot reach each other's `100.x.y.z` Tailscale IPs directly), Moscovo uses a lightweight hub (`moscovo-hub`) as a "mutual friend":

1. **Signaling & Discovery (`:7878`)**: Desktop clients (`moscovo`) connect to `moscovo-hub` over WebSocket (`ws://<hub-ip>:7878/ws`) to join the voice room, receive the peer roster, and exchange WebRTC SDP offers/answers and ICE candidates.
2. **Direct P2P Media**: Clients establish a full-mesh WebRTC (`pion/webrtc/v4`) connection with STUN hole-punching for direct DTLS-SRTP encrypted audio and an SCTP `control` DataChannel.
3. **Embedded TURN Fallback (`:3478`)**: When symmetric NATs or strict firewalls block direct UDP, `moscovo-hub` provides an embedded `pion/turn` STUN/TURN relay bound to the Tailscale interface.

## Features

- **Minimalist Keyboard-First UI**: Compact single-screen Wails v3 desktop window (`360x480`), dark high-contrast layout.
- **Noise Suppression**: Built-in RNNoise (`web-noise-suppressor`) audio worklet toggleable on the fly.
- **Per-Peer Controls**: Individual peer volume adjustment, local peer mute, and link type visibility (`P2P` vs `RELAY`).
- **Cross-Platform Audio**: Native audio support for Linux (PipeWire `wpctl` / PulseAudio `pactl`) and Windows (`winmm.dll`).

## Project Structure

```text
cmd/
  moscovo/          # Wails v3 desktop client
  moscovo-hub/      # Headless WebSocket signaling + embedded STUN/TURN server
internal/
  app/              # Wails v3 VoiceService bindings
  audio/            # Platform audio I/O (Linux & Windows)
  domain/           # Pure-Go domain models, room roster, and signaling rules
  hub/              # Signaling server & embedded TURN server
  transport/        # Reconnecting WS hub client & Pion WebRTC mesh
frontend/           # Minimalist HTML/CSS/TS UI + RNNoise AudioWorklet
plans/              # Architecture and implementation plan
```

## Building

Requires **Go 1.25+**.

```bash
# Build the hub daemon
go build -o bin/moscovo-hub ./cmd/moscovo-hub

# Build the desktop client
go build -o bin/moscovo ./cmd/moscovo
```

## Usage

### 1. Start the Shared Hub (`moscovo-hub`)

On the shared Tailscale server:

```bash
./bin/moscovo-hub
```

Flags:
- `-ip`: IP address to bind signaling and TURN listeners (auto-detects `tailscale0` IPv4 if empty, falls back to `127.0.0.1`)
- `-relay-ip`: External/Tailscale IP advertised by the TURN relay (defaults to `-ip`)
- `-ws-port`: WebSocket signaling listen port (default `7878`)
- `-turn-port`: Embedded STUN/TURN UDP listen port (default `3478`)
- `-realm`: TURN authentication realm (default `moscovo.hub`)

### 2. Start the Desktop Client (`moscovo`)

```bash
./bin/moscovo -hub <hub-tailscale-ip>:7878 -nick alice
```

Flags:
- `-hub`: Shared Tailscale Hub address (`host:port` or `ws://` URL, default `127.0.0.1:7878`)
- `-nick`: Display nickname in the voice room
- `-force-relay`: Force TURN relay transport policy

## Keyboard Shortcuts

| Key | Action |
| --- | --- |
| `Enter` | Join / leave call (or apply hub editor changes) |
| `Space` | Push-to-talk (hold) |
| `m` | Toggle microphone mute |
| `d` | Toggle deafen |
| `n` | Toggle RNNoise noise suppression |
| `j` / `k` or `↓` / `↑` | Select peer |
| `h` / `l` or `-` / `+` | Adjust selected peer volume |
| `x` | Mute / unmute selected peer |
| `/` | Edit Hub address and nickname |
| `Esc` | Leave call / cancel editing |

## Testing

```bash
go test ./...
```
