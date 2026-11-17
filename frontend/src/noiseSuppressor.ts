// @ts-check

import {
  NoiseGateWorkletNode,
  RnnoiseWorkletNode,
  SpeexWorkletNode,
  loadRnnoise,
  loadSpeex,
} from "./vendor/web-noise-suppressor/index.js";

/** @typedef {"off" | "rnnoise" | "speex" | "gate"} NoiseSuppressionMode */

const ASSETS_BASE = "/src/vendor/web-noise-suppressor";
const PCM_WORKLET_URL = "/src/pcmProcessorWorklet.js";

/** @param {string} b64 @returns {Uint8Array} */
function base64ToBytes(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    bytes[i] = bin.charCodeAt(i);
  }
  return bytes;
}

/** @param {ArrayBuffer} buf @returns {string} */
function bytesToBase64(buf) {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) {
    bin += String.fromCharCode(bytes[i]);
  }
  return btoa(bin);
}

/**
 * Manages the client-side Web Audio API noise-suppression graph using
 * `@sapphi-red/web-noise-suppressor` AudioWorklet processors.
 */
export function createNoiseSuppressor() {
  /** @type {AudioContext | null} */
  let ctx = null;
  /** @type {Set<string>} */
  const loadedModules = new Set();
  /** @type {Promise<ArrayBuffer> | null} */
  let rnnoiseWasmPromise = null;
  /** @type {Promise<ArrayBuffer> | null} */
  let speexWasmPromise = null;

  /** @type {AudioWorkletNode | null} */
  let sourceNode = null;
  /** @type {AudioWorkletNode | null} */
  let suppressorNode = null;
  /** @type {AudioWorkletNode | null} */
  let sinkNode = null;
  /** @type {GainNode | null} */
  let zeroGainNode = null;

  /** @type {NoiseSuppressionMode} */
  let currentMode = "off";
  let currentActive = false;
  let transitionSeq = 0;
  /** @type {((b64: string) => void) | null} */
  let processedFrameCb = null;

  /** @returns {AudioContext | null} */
  function ensureAudioContext() {
    if (typeof window === "undefined" || typeof window.AudioContext === "undefined") {
      return null;
    }
    if (!ctx) {
      ctx = new window.AudioContext({ sampleRate: 48000 });
    }
    if (ctx.state === "suspended") {
      void ctx.resume().catch(() => {});
    }
    return ctx;
  }

  /** @param {AudioContext} audioCtx @param {string} moduleUrl */
  async function ensureModule(audioCtx, moduleUrl) {
    if (loadedModules.has(moduleUrl)) {
      return;
    }
    await audioCtx.audioWorklet.addModule(moduleUrl);
    loadedModules.add(moduleUrl);
  }

  /** @returns {Promise<ArrayBuffer>} */
  function getRnnoiseWasm() {
    if (!rnnoiseWasmPromise) {
      rnnoiseWasmPromise = loadRnnoise({
        url: `${ASSETS_BASE}/rnnoise.wasm`,
        simdUrl: `${ASSETS_BASE}/rnnoise_simd.wasm`,
      });
    }
    return rnnoiseWasmPromise;
  }

  /** @returns {Promise<ArrayBuffer>} */
  function getSpeexWasm() {
    if (!speexWasmPromise) {
      speexWasmPromise = loadSpeex({
        url: `${ASSETS_BASE}/speex.wasm`,
      });
    }
    return speexWasmPromise;
  }

  function teardownGraph() {
    if (sourceNode) {
      try {
        sourceNode.port.postMessage({ type: "reset" });
        sourceNode.disconnect();
      } catch {
        // Ignore already-disconnected errors.
      }
      sourceNode = null;
    }
    if (suppressorNode) {
      try {
        if ("destroy" in suppressorNode && typeof suppressorNode.destroy === "function") {
          suppressorNode.destroy();
        }
        suppressorNode.disconnect();
      } catch {
        // Ignore teardown errors.
      }
      suppressorNode = null;
    }
    if (sinkNode) {
      try {
        sinkNode.port.onmessage = null;
        sinkNode.port.postMessage({ type: "reset" });
        sinkNode.disconnect();
      } catch {
        // Ignore teardown errors.
      }
      sinkNode = null;
    }
    if (zeroGainNode) {
      try {
        zeroGainNode.disconnect();
      } catch {
        // Ignore teardown errors.
      }
      zeroGainNode = null;
    }
  }

  /**
   * Switches or tears down the active noise-suppression graph.
   * @param {NoiseSuppressionMode} mode
   * @param {boolean} active
   */
  async function setMode(mode, active) {
    const shouldRun = active && mode !== "off";
    if (mode === currentMode && active === currentActive) {
      if (shouldRun && ctx && ctx.state === "suspended") {
        void ctx.resume().catch(() => {});
      }
      return;
    }

    currentMode = mode;
    currentActive = active;
    transitionSeq += 1;
    const seq = transitionSeq;

    teardownGraph();
    if (!shouldRun) {
      return;
    }

    const audioCtx = ensureAudioContext();
    if (!audioCtx || !audioCtx.audioWorklet) {
      return;
    }

    try {
      await ensureModule(audioCtx, PCM_WORKLET_URL);
      if (seq !== transitionSeq) {
        return;
      }

      /** @type {AudioWorkletNode} */
      let nextSuppressor;
      if (mode === "rnnoise") {
        const [, wasmBinary] = await Promise.all([
          ensureModule(audioCtx, `${ASSETS_BASE}/rnnoiseWorklet.js`),
          getRnnoiseWasm(),
        ]);
        if (seq !== transitionSeq) {
          return;
        }
        nextSuppressor = new RnnoiseWorkletNode(audioCtx, {
          wasmBinary: wasmBinary.slice(0),
          maxChannels: 1,
        });
      } else if (mode === "speex") {
        const [, wasmBinary] = await Promise.all([
          ensureModule(audioCtx, `${ASSETS_BASE}/speexWorklet.js`),
          getSpeexWasm(),
        ]);
        if (seq !== transitionSeq) {
          return;
        }
        nextSuppressor = new SpeexWorkletNode(audioCtx, {
          wasmBinary: wasmBinary.slice(0),
          maxChannels: 1,
        });
      } else {
        await ensureModule(audioCtx, `${ASSETS_BASE}/noiseGateWorklet.js`);
        if (seq !== transitionSeq) {
          return;
        }
        nextSuppressor = new NoiseGateWorkletNode(audioCtx, {
          openThreshold: -50,
          closeThreshold: -60,
          holdMs: 100,
          maxChannels: 1,
        });
      }

      const nextSource = new AudioWorkletNode(audioCtx, "moscovo-pcm-source", {
        numberOfInputs: 0,
        numberOfOutputs: 1,
        outputChannelCount: [1],
      });
      const nextSink = new AudioWorkletNode(audioCtx, "moscovo-pcm-sink", {
        numberOfInputs: 1,
        numberOfOutputs: 1,
        outputChannelCount: [1],
      });
      const nextGain = audioCtx.createGain();
      nextGain.gain.value = 0;

      nextSink.port.onmessage = (event) => {
        if (seq !== transitionSeq) {
          return;
        }
        if (event.data instanceof ArrayBuffer && processedFrameCb) {
          processedFrameCb(bytesToBase64(event.data));
        }
      };

      nextSource.connect(nextSuppressor);
      nextSuppressor.connect(nextSink);
      nextSink.connect(nextGain);
      nextGain.connect(audioCtx.destination);

      sourceNode = nextSource;
      suppressorNode = nextSuppressor;
      sinkNode = nextSink;
      zeroGainNode = nextGain;
    } catch (err) {
      if (seq === transitionSeq) {
        teardownGraph();
      }
      console.warn("noise suppressor init failed, using raw mic fallback:", err);
    }
  }

  /** @param {string} b64 */
  function pushRawMicBase64(b64) {
    if (!b64 || !sourceNode || !currentActive || currentMode === "off") {
      return;
    }
    if (ctx && ctx.state === "suspended") {
      void ctx.resume().catch(() => {});
    }
    const bytes = base64ToBytes(b64);
    sourceNode.port.postMessage(bytes.buffer, [bytes.buffer]);
  }

  /** @param {(b64: string) => void} cb */
  function onProcessedFrame(cb) {
    processedFrameCb = cb;
  }

  return {
    setMode,
    pushRawMicBase64,
    onProcessedFrame,
  };
}
