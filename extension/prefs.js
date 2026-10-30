// Zipline: the extension's settings, and its keyboard shortcuts. Loaded after background.js (importScripts
// in Chrome, listed after it in the Firefox manifest); it uses its helpers.
//
// The settings live in the app (Options → Browser) and come with each ping (config.browser). A copy is
// kept here (storage.local "prefs") for when the app isn't running; a change made here then (the popup's
// toggles, a video button's "Don't show on this site") takes effect at once and goes to the app as soon
// as it answers again ("prefsDirty"). Settings an older version kept here go to the app once.

const LEGACY = { intercept: null, minKB: null, skipExts: null, videoButton: null, noButtonHosts: null };
let prefsChain = Promise.resolve();
const prefsDo = f => (prefsChain = prefsChain.then(f).catch(() => {}));

// syncPrefs runs after each ping that worked: a change made while the app was away goes to it first,
// then the app's settings become the copy here.
function syncPrefs() {
  return prefsDo(async () => {
    const s = await B.storage.local.get({ prefs: null, prefsDirty: false });
    if (s.prefsDirty && s.prefs) {
      const r = await call("settings", s.prefs, 5000);
      if (r && r.ok && r.browser) await B.storage.local.set({ prefs: r.browser, prefsDirty: false });
      return;
    }
    const app = appCfg && appCfg.browser;
    if (app && JSON.stringify(app) !== JSON.stringify(s.prefs)) await B.storage.local.set({ prefs: app });
  });
}

// setPrefs changes some settings here and in the app (later, if it isn't running).
function setPrefs(change) {
  return prefsDo(async () => {
    const cur = { ...DEFAULTS, ...((await B.storage.local.get({ prefs: null })).prefs || {}), ...change };
    await B.storage.local.set({ prefs: cur });
    const r = await call("settings", change, 5000);
    const sent = !!(r && r.ok && r.browser);
    await B.storage.local.set(sent ? { prefs: r.browser, prefsDirty: false } : { prefsDirty: true });
    return { ok: true, sent, prefs: sent ? r.browser : cur };
  });
}

// what an older version kept here goes to the app (once)
B.storage.local.get(LEGACY).then(old => {
  if (Object.values(old).every(v => v == null)) return;
  const change = {};
  if (old.intercept != null) change.takeover = !!old.intercept;
  if (old.minKB != null) change.minKB = +old.minKB || 0;
  if (old.skipExts != null) change.skipTypes = String(old.skipExts);
  if (old.videoButton != null) change.videoButton = !!old.videoButton;
  if (Array.isArray(old.noButtonHosts)) change.noButton = old.noButtonHosts;
  return prefsDo(async () => {
    const cur = { ...DEFAULTS, ...((await B.storage.local.get({ prefs: null })).prefs || {}), ...change };
    await B.storage.local.set({ prefs: cur, prefsDirty: true });
    await B.storage.local.remove(Object.keys(LEGACY));
  }).then(() => appUp && syncPrefs());
}).catch(() => {});

// ---- the popup, and the video button's menu
B.runtime.onMessage.addListener((msg, sender, reply) => {
  switch (msg && msg.type) {
    case "prefs": settings().then(prefs => reply({ prefs, theme: (appCfg && appCfg.theme) || "" })); return true;
    case "set-prefs": setPrefs(msg.change || {}).then(reply); return true;
    case "hide-button": (async () => { // "Don't show on this site"
      const host = String(msg.host || "").toLowerCase(), s = await settings();
      reply(host && !s.noButton.includes(host) ? await setPrefs({ noButton: [...s.noButton, host] }) : { ok: true });
    })(); return true;
    case "show-app": call("show", { page: msg.page || "" }, 3000, { start: true }).then(r => reply({ ok: !!r })); return true; // "Open Zipline", Options → Browser
  }
});

// ---- keyboard shortcuts (manifest "commands"; the browser's own shortcut opens the popup)
// "Download with Zipline": the link under the pointer (links.js notes it), else this page's video.
B.commands && B.commands.onCommand.addListener((name, tab) => { if (name === "zl-download") hotkeyDownload(tab); });
async function hotkeyDownload(tab) {
  if (!tab) [tab] = await B.tabs.query({ active: true, currentWindow: true });
  if (!tab || !/^https?:/i.test(tab.url || "")) return notifyBadge("off");
  let link = null;
  try {
    const res = await B.scripting.executeScript({ target: { tabId: tab.id, allFrames: true }, func: () => globalThis.__ziplineHover || null });
    for (const r of res || []) if (r.result && r.result.url && (!link || r.result.t > link.t)) link = r.result;
  } catch {}
  if (link) {
    if (!(await pingNow()) || !(await send(link.url, link.page || tab.url, "", tab.title || "", await ctxOfTab(tab)))) notifyBadge("off");
    return;
  }
  let src = ""; // (the page's biggest video, as the video button would send it, when nothing else was seen)
  try {
    const res = await B.scripting.executeScript({ target: { tabId: tab.id }, func: () => {
      let best = null;
      for (const v of document.querySelectorAll("video")) {
        const r = v.getBoundingClientRect(), s = v.currentSrc || v.src;
        if (s && !s.startsWith("blob:") && (!best || r.width * r.height > best.a)) best = { s, a: r.width * r.height };
      }
      return best && best.s;
    } });
    src = (res && res[0] && res[0].result) || "";
  } catch {}
  const items = await mediaList(tab.id, src).catch(() => []);
  const r = items.length ? await sendMedia(tab.id, items[0].url, tab.url) : null;
  if (!r || !r.ok) notifyBadge("off");
}
