// AudioWorklet processors bridging Moscovo's 16kHz s16le PCM frames with 48kHz Web Audio noise suppressors.

class MoscovoPcmSourceProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this._capacity = 16384; // ~341ms at 48kHz
    this._ring = new Float32Array(this._capacity);
    this._readIdx = 0;
    this._writeIdx = 0;
    this._count = 0;
    this._prevSample = 0;

    this.port.onmessage = (event) => {
      const data = event.data;
      if (!data) {
        return;
      }
      if (data.type === "reset") {
        this._readIdx = 0;
        this._writeIdx = 0;
        this._count = 0;
        this._prevSample = 0;
        return;
      }
      const buf = data instanceof ArrayBuffer ? data : data.buffer;
      if (!(buf instanceof ArrayBuffer) || buf.byteLength < 2) {
        return;
      }
      const view = new DataView(buf, data.byteOffset || 0, data.byteLength || buf.byteLength);
      const numSamples = Math.floor(view.byteLength / 2);

      // Keep ring buffer bounded to ~100ms (4800 samples) to avoid building latency.
      const maxBuffered = 4800;
      if (this._count + numSamples * 3 > maxBuffered) {
        const excess = this._count + numSamples * 3 - maxBuffered;
        const drop = Math.min(this._count, excess);
        this._readIdx = (this._readIdx + drop) % this._capacity;
        this._count -= drop;
      }

      for (let i = 0; i < numSamples; i++) {
        const s16 = view.getInt16(i * 2, true);
        const cur = s16 / 32768.0;
        const prev = this._prevSample;
        const delta = cur - prev;

        this._pushSample(prev + delta * (1 / 3));
        this._pushSample(prev + delta * (2 / 3));
        this._pushSample(cur);
        this._prevSample = cur;
      }
    };
  }

  _pushSample(v) {
    if (this._count >= this._capacity) {
      this._readIdx = (this._readIdx + 1) % this._capacity;
      this._count -= 1;
    }
    this._ring[this._writeIdx] = v;
    this._writeIdx = (this._writeIdx + 1) % this._capacity;
    this._count += 1;
  }

  process(_inputs, outputs) {
    const outChannels = outputs[0];
    if (!outChannels || outChannels.length === 0) {
      return true;
    }
    const ch0 = outChannels[0];
    for (let i = 0; i < ch0.length; i++) {
      if (this._count > 0) {
        ch0[i] = this._ring[this._readIdx];
        this._readIdx = (this._readIdx + 1) % this._capacity;
        this._count -= 1;
      } else {
        ch0[i] = 0;
      }
    }
    return true;
  }
}

class MoscovoPcmSinkProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this._frameSamples = 320; // 20ms @ 16kHz
    this._outFrame = new Int16Array(this._frameSamples);
    this._outIdx = 0;
    this._decimAccum = 0;
    this._decimCount = 0;

    this.port.onmessage = (event) => {
      if (event.data && event.data.type === "reset") {
        this._outIdx = 0;
        this._decimAccum = 0;
        this._decimCount = 0;
      }
    };
  }

  process(inputs) {
    const inChannels = inputs[0];
    if (!inChannels || inChannels.length === 0) {
      return true;
    }
    const ch0 = inChannels[0];
    if (!ch0) {
      return true;
    }

    for (let i = 0; i < ch0.length; i++) {
      this._decimAccum += ch0[i];
      this._decimCount += 1;
      if (this._decimCount === 3) {
        const avg = this._decimAccum / 3;
        this._decimAccum = 0;
        this._decimCount = 0;

        const clamped = Math.max(-1, Math.min(1, avg));
        const s16 = clamped < 0 ? Math.round(clamped * 32768) : Math.round(clamped * 32767);
        this._outFrame[this._outIdx] = s16;
        this._outIdx += 1;

        if (this._outIdx >= this._frameSamples) {
          const copy = new Int16Array(this._outFrame);
          this.port.postMessage(copy.buffer, [copy.buffer]);
          this._outIdx = 0;
        }
      }
    }
    return true;
  }
}

registerProcessor("moscovo-pcm-source", MoscovoPcmSourceProcessor);
registerProcessor("moscovo-pcm-sink", MoscovoPcmSinkProcessor);
