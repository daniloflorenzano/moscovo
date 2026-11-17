// @ts-check

import { createNoiseSuppressor } from "./noiseSuppressor.ts";

/** @typedef {import("./noiseSuppressor.ts").NoiseSuppressionMode} NoiseSuppressionMode */

/**
 * @typedef {Object} PeerSession
 * @property {string} id
 * @property {string} name
 * @property {string} state
 * @property {number} volume
 * @property {boolean} localMuted
 * @property {boolean} remoteMuted
 * @property {boolean} speaking
 * @property {number} rttMs
 */

/**
 * @typedef {Object} AppState
 * @property {string} hubAddr
 * @property {string} localId
 * @property {string} nickname
 * @property {string} hubStatus
 * @property {string} micDevice
 * @property {string} outputDevice
 * @property {boolean} inCall
 * @property {boolean} muted
 * @property {boolean} deafened
 * @property {boolean} pushToTalk
 * @property {boolean} speaking
 * @property {NoiseSuppressionMode} noiseSuppression
 * @property {string} [lastError]
 * @property {PeerSession[]} peers
 */

const SERVICE_FQN = "moscovo/internal/app.VoiceService.";

/** @type {AppState} */
let state = {
  hubAddr: "127.0.0.1:7878",
  localId: "",
  nickname: "user",
  hubStatus: "OFFLINE",
  micDevice: "Default Microphone",
  outputDevice: "Default Output",
  inCall: false,
  muted: false,
  deafened: false,
  pushToTalk: false,
  speaking: false,
  noiseSuppression: "rnnoise",
  lastError: "",
  peers: [],
};

let selectedPeerId = "";
let editingHub = false;
let pttHeld = false;
let callCounter = 0;

const noiseSuppressor = createNoiseSuppressor();

const el = {
  hubSummary: /** @type {HTMLElement} */ (document.getElementById("hub-summary")),
  statusDot: /** @type {HTMLElement} */ (document.getElementById("status-dot")),
  hubAddrDisplay: /** @type {HTMLElement} */ (document.getElementById("hub-addr-display")),
  statusBadge: /** @type {HTMLElement} */ (document.getElementById("status-badge")),
  hubEditor: /** @type {HTMLFormElement} */ (document.getElementById("hub-editor")),
  hubInput: /** @type {HTMLInputElement} */ (document.getElementById("hub-input")),
  nickInput: /** @type {HTMLInputElement} */ (document.getElementById("nick-input")),
  cancelEditBtn: /** @type {HTMLButtonElement} */ (document.getElementById("cancel-edit-btn")),
  errorBanner: /** @type {HTMLElement} */ (document.getElementById("error-banner")),
  micDeviceRow: /** @type {HTMLElement} */ (document.getElementById("mic-device-row")),
  micDeviceName: /** @type {HTMLElement} */ (document.getElementById("mic-device-name")),
  outDeviceRow: /** @type {HTMLElement} */ (document.getElementById("out-device-row")),
  outDeviceName: /** @type {HTMLElement} */ (document.getElementById("out-device-name")),
  roomMeta: /** @type {HTMLElement} */ (document.getElementById("room-meta")),
  peerList: /** @type {HTMLElement} */ (document.getElementById("peer-list")),
  callBtn: /** @type {HTMLButtonElement} */ (document.getElementById("call-btn")),
  muteBtn: /** @type {HTMLButtonElement} */ (document.getElementById("mute-btn")),
  deafenBtn: /** @type {HTMLButtonElement} */ (document.getElementById("deafen-btn")),
  nsBtn: /** @type {HTMLButtonElement} */ (document.getElementById("ns-btn")),
};

/**
 * Invokes a Wails v3 bound Go method via /wails/runtime, falling back to /api.
 * @param {string} method
 * @param {any[]} [args]
 * @param {string} [apiPath]
 * @param {any} [apiBody]
 * @returns {Promise<any>}
 */
async function invokeService(method, args = [], apiPath = "", apiBody = undefined) {
  callCounter += 1;
  const callID = `m-${Date.now()}-${callCounter}`;
  let rpcRejected = false;
  try {
    const resp = await fetch("/wails/runtime", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        object: 0,
        method: 0,
        args: {
          "call-id": callID,
          methodName: SERVICE_FQN + method,
          args,
        },
      }),
    });
    if (resp.ok) {
      return await resp.json();
    }
    if (resp.status === 422) {
      rpcRejected = true;
      const errBody = await resp.json().catch(() => ({ message: "RPC call failed" }));
      throw new Error(errBody.message || "RPC call failed");
    }
  } catch (err) {
    if (rpcRejected) {
      throw err;
    }
    // Fall through to /api HTTP fallback if /wails/runtime is unavailable.
  }

  if (!apiPath) {
    throw new Error(`RPC ${method} failed`);
  }
  const init = apiBody === undefined
    ? { method: apiPath === "/state" ? "GET" : "POST" }
    : {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(apiBody),
      };
  const fallbackResp = await fetch("/api" + apiPath, init);
  const data = await fallbackResp.json();
  if (!fallbackResp.ok) {
    throw new Error(data.error || "request failed");
  }
  return data;
}

async function quitApplication() {
  try {
    await invokeService("Shutdown", [], "/call/leave");
  } catch {
    // Best-effort leave before closing window.
  }
  try {
    await fetch("/wails/runtime", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ object: 2, method: 2 }),
    });
  } catch {
    window.close();
  }
}

function setupWailsEvents() {
  const wailsObj = /** @type {any} */ (window)._wails || {};
  const prevDispatch = wailsObj.dispatchWailsEvent;

  wailsObj.dispatchWailsEvent = (/** @type {{name: string, data: any}} */ ev) => {
    if (typeof prevDispatch === "function") {
      prevDispatch(ev);
    }
    if (!ev || !ev.name) {
      return;
    }
    if (ev.name === "hub:state" || ev.name === "room:roster") {
      if (ev.data && typeof ev.data === "object") {
        applyState(ev.data);
      }
    } else if (ev.name === "peer:stats" && Array.isArray(ev.data)) {
      state.peers = ev.data;
      ensureSelection();
      render();
    } else if (ev.name === "audio:mic-raw" && typeof ev.data === "string") {
      noiseSuppressor.pushRawMicBase64(ev.data);
    }
  };

  /** @type {any} */ (window)._wails = wailsObj;

  function notifyRuntimeReady() {
    const w = /** @type {any} */ (window)._wails;
    if (w && typeof w.invoke === "function") {
      w.invoke("wails:runtime:ready");
    } else {
      setTimeout(notifyRuntimeReady, 30);
    }
  }
  notifyRuntimeReady();
}

/** @param {AppState} next */
function applyState(next) {
  state = {
    ...state,
    ...next,
    peers: Array.isArray(next.peers) ? next.peers : [],
  };
  const micActive = state.inCall && !state.deafened && (!state.muted || state.pushToTalk);
  void noiseSuppressor.setMode(state.noiseSuppression || "rnnoise", micActive);
  ensureSelection();
  render();
}

function ensureSelection() {
  if (state.peers.length === 0) {
    selectedPeerId = "";
    return;
  }
  const stillPresent = state.peers.some((p) => p.id === selectedPeerId);
  if (!stillPresent) {
    selectedPeerId = state.peers[0].id;
  }
}

function render() {
  el.hubAddrDisplay.textContent = `hub: ${state.hubAddr}`;

  const peerTotal = state.peers.length + (state.inCall ? 1 : 0);
  const connectedCount = state.peers.filter((p) => p.state === "P2P" || p.state === "TURN").length + (state.inCall ? 1 : 0);

  el.statusDot.className = "status-dot";
  el.statusBadge.className = "status-badge";

  if (state.hubStatus === "IN CALL") {
    el.statusDot.classList.add("online");
    el.statusBadge.classList.add("online");
    el.statusBadge.textContent = `[IN CALL • ${connectedCount}/${peerTotal}]`;
  } else if (state.hubStatus === "RECONNECTING" || state.hubStatus === "CONNECTING") {
    el.statusDot.classList.add("warn");
    el.statusBadge.classList.add("warn");
    el.statusBadge.textContent = `[${state.hubStatus}]`;
  } else {
    el.statusBadge.textContent = "[OFFLINE]";
  }

  if (state.lastError) {
    el.statusDot.classList.add("err");
    el.errorBanner.textContent = state.lastError;
    el.errorBanner.classList.remove("hidden");
  } else {
    el.errorBanner.textContent = "";
    el.errorBanner.classList.add("hidden");
  }

  const micName = state.micDevice || "Default Microphone";
  const outName = state.outputDevice || "Default Output";
  el.micDeviceName.textContent = micName;
  el.micDeviceRow.title = `Microphone: ${micName}`;
  el.outDeviceName.textContent = outName;
  el.outDeviceRow.title = `Output Audio: ${outName}`;

  el.roomMeta.textContent = state.inCall ? `${state.peers.length} remote` : "idle";

  el.callBtn.textContent = state.inCall ? "[ENTER] LEAVE" : "[ENTER] JOIN";
  el.callBtn.className = state.inCall ? "btn btn-danger" : "btn btn-primary";

  if (state.deafened) {
    el.muteBtn.textContent = "[M] MIC: DEAF";
    el.muteBtn.className = "btn btn-danger";
  } else if (state.muted && state.pushToTalk) {
    el.muteBtn.textContent = "[M] MIC: PTT";
    el.muteBtn.className = "btn btn-warn";
  } else if (state.muted) {
    el.muteBtn.textContent = "[M] MIC: OFF";
    el.muteBtn.className = "btn btn-danger";
  } else {
    el.muteBtn.textContent = "[M] MIC: ON";
    el.muteBtn.className = "btn btn-primary";
  }

  el.deafenBtn.textContent = state.deafened ? "[D] UNDEAF" : "[D] DEAFEN";
  el.deafenBtn.className = state.deafened ? "btn btn-danger" : "btn";

  const nsMode = state.noiseSuppression || "rnnoise";
  if (nsMode === "off") {
    el.nsBtn.textContent = "[N] NS: OFF";
    el.nsBtn.className = "btn";
  } else if (nsMode === "speex") {
    el.nsBtn.textContent = "[N] NS: SPEEX";
    el.nsBtn.className = "btn btn-primary";
  } else if (nsMode === "gate") {
    el.nsBtn.textContent = "[N] NS: GATE";
    el.nsBtn.className = "btn btn-warn";
  } else {
    el.nsBtn.textContent = "[N] NS: RNNOISE";
    el.nsBtn.className = "btn btn-primary";
  }

  renderPeers();
}

function renderPeers() {
  el.peerList.replaceChildren();

  const selfActive = state.inCall && !state.deafened && (!state.muted || state.pushToTalk);
  const selfTalking = selfActive && state.speaking;
  const selfRow = document.createElement("div");
  selfRow.className = "peer-row self" + (selfTalking ? " talking" : "");
  const selfDot = document.createElement("span");
  selfDot.className = "peer-dot" + (selfTalking ? " speaking" : "");
  selfDot.textContent = selfTalking ? "◉" : "○";

  const selfName = document.createElement("span");
  selfName.className = "peer-name";
  selfName.textContent = `you (${state.nickname})`;

  const selfMeta = document.createElement("span");
  selfMeta.className = "peer-meta";
  const nsMode = state.noiseSuppression || "rnnoise";
  const nsSuffix = nsMode !== "off" ? ` · ${nsMode.toUpperCase()}` : "";
  if (state.deafened) {
    selfMeta.textContent = "DEAFENED";
  } else if (state.muted && state.pushToTalk) {
    selfMeta.textContent = `MIC: PTT${nsSuffix}`;
  } else if (state.muted) {
    selfMeta.textContent = "MIC: MUTED";
  } else {
    selfMeta.textContent = `MIC: ON${nsSuffix}`;
  }

  selfRow.append(selfDot, selfName, selfMeta);
  el.peerList.appendChild(selfRow);

  if (state.peers.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-hint";
    empty.textContent = state.inCall
      ? "Waiting for peers on Shared Hub..."
      : "Press Enter to join voice room.";
    el.peerList.appendChild(empty);
    return;
  }

  for (const peer of state.peers) {
    const peerTalking = peer.speaking && !peer.remoteMuted && !peer.localMuted;
    const row = document.createElement("div");
    row.className = "peer-row" + (peer.id === selectedPeerId ? " selected" : "") + (peerTalking ? " talking" : "");
    row.addEventListener("click", () => {
      selectedPeerId = peer.id;
      renderPeers();
    });

    const dot = document.createElement("span");
    dot.className = "peer-dot" + (peerTalking ? " speaking" : "");
    dot.textContent = peerTalking ? "◉" : "○";

    const name = document.createElement("span");
    name.className = "peer-name";
    name.textContent = peer.name;

    const meta = document.createElement("span");
    meta.className = "peer-meta";

    const pathBadge = document.createElement("span");
    if (peer.state === "P2P") {
      pathBadge.className = "badge-p2p";
      pathBadge.textContent = "[P2P]";
    } else if (peer.state === "TURN") {
      pathBadge.className = "badge-turn";
      pathBadge.textContent = "[TURN]";
    } else {
      pathBadge.className = "badge-wait";
      pathBadge.textContent = `[${peer.state || "WAIT"}]`;
    }

    const rtt = document.createElement("span");
    rtt.textContent = peer.rttMs > 0 ? `${peer.rttMs}ms` : "--ms";

    const vol = document.createElement("span");
    vol.textContent = `${peer.volume}%`;

    meta.append(pathBadge, rtt, vol);

    if (peer.localMuted || peer.remoteMuted) {
      const muteTag = document.createElement("span");
      muteTag.className = "badge-muted";
      muteTag.textContent = peer.localMuted ? "🔇" : "[MIC OFF]";
      meta.appendChild(muteTag);
    }

    row.append(dot, name, meta);
    el.peerList.appendChild(row);
  }
}

function openHubEditor() {
  editingHub = true;
  el.hubInput.value = state.hubAddr;
  el.nickInput.value = state.nickname;
  el.hubEditor.classList.remove("hidden");
  el.hubInput.focus();
  el.hubInput.select();
}

function closeHubEditor() {
  editingHub = false;
  el.hubEditor.classList.add("hidden");
}

async function runAction(fn) {
  try {
    const next = await fn();
    if (next && typeof next === "object") {
      applyState(next);
    }
  } catch (err) {
    state.lastError = err instanceof Error ? err.message : String(err);
    render();
  }
}

function moveSelection(delta) {
  if (state.peers.length === 0) {
    return;
  }
  const idx = state.peers.findIndex((p) => p.id === selectedPeerId);
  const nextIdx = idx < 0 ? 0 : Math.max(0, Math.min(state.peers.length - 1, idx + delta));
  selectedPeerId = state.peers[nextIdx].id;
  renderPeers();
}

function adjustSelectedVolume(delta) {
  const peer = state.peers.find((p) => p.id === selectedPeerId);
  if (!peer) {
    return;
  }
  const nextVol = Math.max(0, Math.min(200, peer.volume + delta));
  void runAction(() => invokeService("SetPeerVolume", [peer.id, nextVol], "/peer/volume", { peerId: peer.id, volume: nextVol }));
}

function wireControls() {
  el.hubSummary.addEventListener("click", () => {
    if (editingHub) {
      closeHubEditor();
    } else {
      openHubEditor();
    }
  });

  el.cancelEditBtn.addEventListener("click", closeHubEditor);

  el.hubEditor.addEventListener("submit", (e) => {
    e.preventDefault();
    const hubAddr = el.hubInput.value.trim();
    const nickname = el.nickInput.value.trim();
    closeHubEditor();
    void runAction(() => invokeService("SetHubConfig", [hubAddr, nickname], "/config", { hubAddr, nickname }));
  });

  el.callBtn.addEventListener("click", () => {
    void runAction(() => invokeService("ToggleCall", [], "/call/toggle"));
  });

  el.muteBtn.addEventListener("click", () => {
    void runAction(() => invokeService("ToggleMute", [], "/mic/mute"));
  });

  el.deafenBtn.addEventListener("click", () => {
    void runAction(() => invokeService("ToggleDeafen", [], "/mic/deafen"));
  });

  el.nsBtn.addEventListener("click", () => {
    void runAction(() => invokeService("CycleNoiseSuppression", [], "/mic/ns"));
  });

  window.addEventListener("keydown", (e) => {
    if (e.ctrlKey && e.key.toLowerCase() === "q") {
      e.preventDefault();
      void quitApplication();
      return;
    }

    if (editingHub) {
      if (e.key === "Escape") {
        e.preventDefault();
        closeHubEditor();
      }
      return;
    }

    const activeTag = document.activeElement?.tagName;
    if (activeTag === "INPUT" || activeTag === "TEXTAREA") {
      return;
    }

    switch (e.key) {
      case "Enter":
        e.preventDefault();
        void runAction(() => invokeService("ToggleCall", [], "/call/toggle"));
        break;
      case " ":
        e.preventDefault();
        if (!state.inCall && !e.repeat) {
          void runAction(() => invokeService("JoinCall", [], "/call/join"));
        } else if (state.inCall && state.muted && !pttHeld && !e.repeat) {
          pttHeld = true;
          void runAction(() => invokeService("SetPushToTalk", [true], "/mic/ptt", { active: true }));
        }
        break;
      case "m":
      case "M":
        e.preventDefault();
        void runAction(() => invokeService("ToggleMute", [], "/mic/mute"));
        break;
      case "d":
      case "D":
        e.preventDefault();
        void runAction(() => invokeService("ToggleDeafen", [], "/mic/deafen"));
        break;
      case "n":
      case "N":
        e.preventDefault();
        void runAction(() => invokeService("CycleNoiseSuppression", [], "/mic/ns"));
        break;
      case "j":
      case "ArrowDown":
        e.preventDefault();
        moveSelection(1);
        break;
      case "k":
      case "ArrowUp":
        e.preventDefault();
        moveSelection(-1);
        break;
      case "h":
      case "-":
        e.preventDefault();
        adjustSelectedVolume(-10);
        break;
      case "l":
      case "+":
      case "=":
        e.preventDefault();
        adjustSelectedVolume(10);
        break;
      case "x":
      case "X":
        e.preventDefault();
        if (selectedPeerId) {
          const pid = selectedPeerId;
          void runAction(() => invokeService("TogglePeerMute", [pid], "/peer/mute", { peerId: pid }));
        }
        break;
      case "/":
        e.preventDefault();
        openHubEditor();
        break;
      case "Escape":
        e.preventDefault();
        if (state.inCall) {
          void runAction(() => invokeService("LeaveCall", [], "/call/leave"));
        }
        break;
    }
  });

  window.addEventListener("keyup", (e) => {
    if (e.key === " " && pttHeld) {
      pttHeld = false;
      void runAction(() => invokeService("SetPushToTalk", [false], "/mic/ptt", { active: false }));
    }
  });

  window.addEventListener("blur", () => {
    if (pttHeld) {
      pttHeld = false;
      void runAction(() => invokeService("SetPushToTalk", [false], "/mic/ptt", { active: false }));
    }
  });
}

let syncInFlight = false;

async function syncState() {
  if (syncInFlight) {
    return;
  }
  syncInFlight = true;
  try {
    const next = await invokeService("GetState", [], "/state");
    if (next && typeof next === "object") {
      applyState(next);
    }
  } catch {
    // Keep current rendered state if a background poll fails transiently.
  } finally {
    syncInFlight = false;
  }
}

async function init() {
  noiseSuppressor.onProcessedFrame((b64) => {
    void invokeService("SubmitProcessedMicFrame", [b64], "/mic/frame", { pcm: b64 }).catch(() => {});
  });
  setupWailsEvents();
  wireControls();
  render();
  await runAction(() => invokeService("GetState", [], "/state"));
  setInterval(() => {
    void syncState();
  }, 500);
}

void init();
