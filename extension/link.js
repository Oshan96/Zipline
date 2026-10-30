// Zipline link: how the extension reaches the app. Loaded before background.js (importScripts in Chrome, listed
// first in Firefox's manifest); background.js's call() and ping() go through zlLink.call() and zlLink.ping().
//  1. Native messaging first: the browser starts Zipline's own host program, which only this extension may talk to.
//     The host finds the app through a file only this user can read, starts Zipline when it isn't running for
//     something the user did (opts.start: Download with Zipline, Open Zipline…, never a check-in or background
//     work) and passes on the app's wake-ups, so the extension checks in at once. It goes when Zipline isn't
//     running, and isn't started again just to hear that: a check-in first looks whether Zipline's port answers.
//  2. Without the host (Zipline sets it up each time it starts): HTTP on 127.0.0.1:17890, paired. Zipline's window
//     asks once whether to let this browser in; every request is then signed with the key it gives, and a reply
//     only counts with the app's proof, so a program that takes the port can't use or learn the key.
// Every reply says the app's version and the oldest extension it works with; when one side is too old for the
// other, the toolbar button shows "!" and the popup says which side needs updating (linkpopup.js).
(() => {
  const LB = globalThis.browser ?? globalThis.chrome;
  const HOST = "app.zipline.host";
  const HTTP = "http://127.0.0.1:17890";
  const VERSION = LB.runtime.getManifest().version;
  const MIN_APP = "1.0"; // the oldest Zipline this extension works with
  const enc = new TextEncoder();
  const hex = b => [...new Uint8Array(b)].map(x => x.toString(16).padStart(2, "0")).join("");
  const randHex = n => hex(crypto.getRandomValues(new Uint8Array(n)));
  const older = (a, b) => { // dotted versions by number: "1.6.2" < "1.10"
    const x = String(a).split("."), y = String(b).split(".");
    for (let i = 0; i < Math.max(x.length, y.length); i++) { const d = (+x[i] || 0) - (+y[i] || 0); if (d) return d < 0; }
    return false;
  };
  const keys = new Map();
  async function hmac(key, text) {
    let k = keys.get(key);
    if (!k) { k = await crypto.subtle.importKey("raw", enc.encode(key), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]); keys.set(key, k); }
    return hex(await crypto.subtle.sign("HMAC", k, enc.encode(text)));
  }
  const sha256 = async text => hex(await crypto.subtle.digest("SHA-256", enc.encode(text)));
  const wake = () => { if (typeof globalThis.ping === "function") globalThis.ping(); }; // (background.js)

  // ---- what the popup shows (storage.local "zlLink"): up (the app answered), via (native / http), host (the native
  // host is there), the versions, need ("app" or "ext": which side is too old) and pair (HTTP pairing: needed,
  // pending, denied, rejected)
  let state = { up: false, via: "", host: false, app: "", minExt: "", minApp: MIN_APP, version: VERSION, need: "", pair: "" };
  let saved = "", badged = false;
  async function setState(change) {
    state = { ...state, ...change };
    const s = JSON.stringify(state);
    if (s !== saved) { saved = s; await LB.storage.local.set({ zlLink: state }).catch(() => {}); }
    if (state.need) { // (again each time: other code clears the badge after its own messages)
      LB.action.setBadgeBackgroundColor({ color: "#d93a3a" });
      LB.action.setBadgeText({ text: "!" });
      LB.action.setTitle({ title: state.need === "app" ? "Zipline needs updating" : "The Zipline extension needs updating" });
    } else if (badged) { LB.action.setBadgeText({ text: "" }); LB.action.setTitle({ title: "Zipline" }); }
    badged = !!state.need;
  }
  // handshake: the app's version and the oldest extension it takes, from a reply's headers or the host's envelope
  function handshake(app, minExt, via) {
    const need = !app || older(app, MIN_APP) ? "app" : minExt && older(VERSION, minExt) ? "ext" : "";
    return setState({ up: true, via, app: app || "", minExt: minExt || "", need, ...(via === "native" ? { pair: "" } : {}) });
  }

  // ---- native messaging
  let port = null, portUp = false, nativeOff = 0, seq = 0;
  const waiting = new Map(); // request id -> { done, lost, extend }
  function native() {
    if (port) return port;
    if (Date.now() < nativeOff || !LB.runtime.connectNative) return null;
    let p;
    try { p = LB.runtime.connectNative(HOST); } catch { nativeOff = Date.now() + 60000; return null; }
    port = p; portUp = false;
    p.onMessage.addListener(m => {
      if (!m) return;
      if (!portUp) { portUp = true; setState({ host: true }); }
      if (m.type === "wake") return wake();
      const w = m.id != null && waiting.get(m.id);
      if (!w) return;
      if (m.type === "starting") return w.extend(); // Zipline is starting: up to 10 seconds more
      w.done(m);
    });
    p.onDisconnect.addListener(() => {
      void LB.runtime.lastError;
      if (port === p) port = null;
      const worked = portUp;
      portUp = false;
      if (!worked) { nativeOff = Date.now() + 60000; setState({ host: false }); } // no host here (yet): HTTP for a minute
      for (const w of [...waiting.values()]) w.lost(worked);
      // (the host went: Zipline quit, or someone ended it. It's started again only when a call needs it, never
      // straight away: that brought a Zipline process back each time it was ended in Task Manager)
    });
    return p;
  }
  function viaNative(p, path, body, timeout, start) {
    return new Promise(resolve => {
      const id = ++seq;
      let timer = 0;
      const finish = v => { clearTimeout(timer); waiting.delete(id); resolve(v); };
      const arm = ms => { clearTimeout(timer); timer = setTimeout(() => finish({ timeout: true }), ms); };
      waiting.set(id, { done: m => finish({ m }), lost: worked => finish({ lost: true, worked }), extend: () => arm(Math.max(timeout, 12000)) });
      arm(timeout);
      try { p.postMessage({ id, path, body, timeout, start: !!start, leave: true, ver: VERSION, minApp: MIN_APP }); } catch { finish({ lost: true, worked: portUp }); }
    });
  }

  // ---- HTTP, paired
  async function viaHttp(path, body, timeout) {
    const { zlPair: pair } = await LB.storage.local.get({ zlPair: null });
    const method = body === undefined ? "GET" : "POST", data = body === undefined ? "" : JSON.stringify(body);
    const uri = "/api/ext/" + path, nonce = randHex(16), ts = String(Date.now());
    const headers = { "X-Zipline-Ext-Version": VERSION, "X-Zipline-Min-App": MIN_APP };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (pair && pair.id && pair.key) {
      Object.assign(headers, { "X-Zipline-Pair": pair.id, "X-Zipline-Time": ts, "X-Zipline-Nonce": nonce,
        "X-Zipline-Sig": await hmac(pair.key, [method, uri, ts, nonce, await sha256(data)].join("\n")) });
    }
    const c = new AbortController(), t = setTimeout(() => c.abort(), timeout);
    try {
      const r = await fetch(HTTP + uri, { method, headers, body: body === undefined ? undefined : data, signal: c.signal, credentials: "omit", cache: "no-store" });
      const app = r.headers.get("X-Zipline-Version");
      if (!app) { // an older Zipline (it doesn't take part in the handshake), or not Zipline at all
        const old = await r.json().then(j => !!j && j.app === "zipline", () => false);
        await setState({ up: false, via: "", ...(old ? { need: "app", app: "" } : {}) });
        return null;
      }
      if (pair && pair.key) {
        if (r.headers.get("X-Zipline-Proof") !== await hmac(pair.key, "zipline-proof\n" + nonce)) {
          // no proof: Zipline forgot this browser (Options → Browser), or another program has the port. Nothing is
          // re-paired by itself: Connect in the popup asks again.
          await setState({ up: false, via: "", pair: r.status === 401 ? "rejected" : state.pair });
          return null;
        }
        await handshake(app, r.headers.get("X-Zipline-Min-Ext"), "http");
        return r.ok ? await r.json() : null;
      }
      await setState({ up: true, via: "", app, minExt: r.headers.get("X-Zipline-Min-Ext") || "" });
      autoPair(); // not paired yet: Zipline's window asks (once a day by itself; Connect in the popup any time)
      return null;
    } catch { await setState({ up: false, via: "" }); return null; } finally { clearTimeout(t); }
  }
  async function pairPost(body) {
    try {
      const r = await fetch(HTTP + "/api/ext/pair", { method: "POST", headers: { "Content-Type": "application/json", "X-Zipline-Ext-Version": VERSION, "X-Zipline-Min-App": MIN_APP },
        body: JSON.stringify(body), credentials: "omit", cache: "no-store" });
      return r.ok && r.headers.get("X-Zipline-Version") ? await r.json() : null;
    } catch { return null; }
  }
  const browserName = () => {
    const ua = navigator.userAgent;
    return /Firefox\//.test(ua) ? "Firefox" : /Edg\//.test(ua) ? "Microsoft Edge" : /OPR\//.test(ua) ? "Opera" : /Vivaldi/.test(ua) ? "Vivaldi"
      : navigator.brave ? "Brave" : (navigator.userAgentData?.brands || []).some(b => b.brand === "Google Chrome") ? "Google Chrome" : "Chromium";
  };
  let pairTimer = 0;
  async function autoPair() {
    const s = await LB.storage.local.get({ zlPairAsk: null, zlPairAuto: 0 });
    if (s.zlPairAsk) return pollPair();
    if (Date.now() - s.zlPairAuto < 86400000) return setState({ pair: state.pair === "denied" ? "denied" : "needed" });
    await LB.storage.local.set({ zlPairAuto: Date.now() });
    askPair();
  }
  async function askPair() {
    clearTimeout(pairTimer);
    await LB.storage.local.remove(["zlPair", "zlPairAsk"]);
    const r = await pairPost({ version: VERSION, browser: browserName() });
    if (!r || !r.id) return setState({ pair: r && r.state === "denied" ? "denied" : "needed" });
    await LB.storage.local.set({ zlPairAsk: { id: r.id, at: Date.now() } });
    await setState({ pair: "pending" });
    pairTimer = setTimeout(pollPair, 1500);
  }
  async function pollPair() {
    clearTimeout(pairTimer);
    const { zlPairAsk: ask } = await LB.storage.local.get({ zlPairAsk: null });
    if (!ask) return;
    const r = await pairPost({ id: ask.id });
    if (r && r.state === "allowed" && r.pair && r.key) {
      await LB.storage.local.set({ zlPair: { id: r.pair, key: r.key } });
      await LB.storage.local.remove("zlPairAsk");
      await setState({ pair: "" });
      return wake();
    }
    if (r && r.state === "pending" && Date.now() - ask.at < 130000) { await setState({ pair: "pending" }); pairTimer = setTimeout(pollPair, 1500); return; }
    await LB.storage.local.remove("zlPairAsk");
    await setState({ pair: r && r.state === "denied" ? "denied" : "needed" });
  }

  // listening: whether anything answers on Zipline's port. With no host running, a call that may not start
  // Zipline looks here first, so the browser doesn't start a Zipline process just to hear Zipline isn't running.
  // (Something else on the port says yes: Zipline then took another port, and the host finds it.)
  async function listening() {
    const c = new AbortController(), t = setTimeout(() => c.abort(), 800);
    try { await fetch(HTTP + "/", { method: "HEAD", signal: c.signal, credentials: "omit", cache: "no-store" }); return true; }
    catch { return false; } finally { clearTimeout(t); }
  }

  // call(path, body, timeout): the app's JSON reply, or null (not reachable, refused, timed out). opts.start: the
  // native host may start Zipline for it; only for something the user did, never a check-in or background work.
  async function call(path, body, timeout = 800, opts = {}) {
    if (!port && !opts.start && !(await listening())) { await setState({ up: false, via: "" }); return null; }
    let p = native();
    if (p) {
      let r = await viaNative(p, path, body, timeout, opts.start);
      if (r.lost && opts.start && (p = native())) r = await viaNative(p, path, body, timeout, true); // (the host was just going: a fresh one)
      if (r.m) {
        if (!r.m.status) { await setState({ up: false, via: "" }); return null; } // Zipline isn't running (or didn't start)
        await handshake(r.m.app, r.m.minExt, "native");
        return r.m.status >= 200 && r.m.status < 300 ? (r.m.body ?? {}) : null;
      }
      if (r.timeout || r.worked) return null; // the host is there; the app was too slow
    }
    return viaHttp(path, body, timeout); // no native host on this computer
  }

  // The popup's buttons: Connect (ask Zipline's window to let this browser in) and Start Zipline (through the host).
  LB.storage.onChanged.addListener((ch, area) => {
    if (area !== "local") return;
    if (ch.zlPairNow) askPair();
    if (ch.zlStartNow) call("ping", undefined, 12000, { start: true }).then(r => r && wake());
  });
  LB.storage.local.get({ zlPairAsk: null }).then(s => s.zlPairAsk && pollPair()).catch(() => {}); // (a worker that slept mid-way)

  globalThis.zlLink = {
    call,
    // ping: the check-in; start lets the native host start Zipline when it isn't running
    ping: start => call("ping", undefined, start ? 12000 : 800, { start: !!start }),
    state: () => ({ ...state }),
  };
})();
