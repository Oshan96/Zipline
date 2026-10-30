// Zipline Download Catcher — hands links, videos and (if you switch it on) browser downloads to the Zipline app.
// Works in Chrome, Edge, Brave, Opera (chrome.*) and Firefox (browser.*).
const B = globalThis.browser ?? globalThis.chrome;
// The link to the app (native messaging, else paired HTTP): link.js. Chrome loads it here; Firefox lists it first.
try { importScripts("link.js"); } catch {}
// The extension's settings live in the app (Options → Browser) and come with each ping; prefs.js keeps a
// copy for when the app isn't running. Taking over browser downloads is OFF unless the user turns it on.
const DEFAULTS = { takeover: false, minKB: 256, skipTypes: "html htm php asp aspx json xml js css", videoButton: true, noButton: [] };

// Sites whose videos are DRM-protected or whose terms don't allow downloading. The app's own list (it sends it
// with each ping, so there is one list) replaces this copy; this one is for an older or stopped app.
const BLOCKED = ["youtube.com", "youtu.be", "youtube-nocookie.com", "googlevideo.com", "netflix.com", "nflxvideo.net",
  "disneyplus.com", "primevideo.com", "amazonvideo.com", "hulu.com", "max.com", "hbomax.com", "spotify.com",
  "tv.apple.com", "peacocktv.com", "paramountplus.com", "crunchyroll.com"];
const BLOCKED_MSG = "Zipline doesn't download from YouTube or DRM-protected streaming services";
let blockedList = BLOCKED;
const isBlockedHost = h => !!h && blockedList.some(b => h === b || h.endsWith("." + b));
function useAppList(cfg) {
  const l = cfg && Array.isArray(cfg.protected) ? cfg.protected.map(x => String(x).trim().toLowerCase().replace(/^\*?\./, "")).filter(Boolean) : [];
  blockedList = l.length ? l : BLOCKED;
}
const blockedMsg = () => (appCfg && appCfg.protectedMsg) || BLOCKED_MSG;
const hostOf = u => { try { return new URL(u).hostname.toLowerCase(); } catch { return ""; } };

async function settings() {
  const s = { ...DEFAULTS, ...((await B.storage.local.get({ prefs: null })).prefs || {}) };
  if (!Array.isArray(s.noButton)) s.noButton = [];
  return s;
}

// call: the app's JSON reply, or null. (link.js: through the native host, or signed over HTTP once paired)
async function call(path, body, timeout = 800, opts = {}) { // opts.start: something the user did, which may start Zipline (link.js)
  return zlLink.call(path, body, timeout, opts);
}

// The app sends its browser settings (hotkeys, sites to leave alone) with each ping, and the
// pages to open when a download's link has expired.
let appCfg = null;
async function ping(start) { // start: the native host may start Zipline when it isn't running (link.js)
  const r = await zlLink.ping(start);
  if (r && r.config && JSON.stringify(r.config) !== JSON.stringify(appCfg)) {
    appCfg = r.config;
    useAppList(appCfg);
    B.storage.local.set({ appCfg });
  }
  if (r && Array.isArray(r.refresh)) r.refresh.forEach(startRefresh);
  if (r && Array.isArray(r.grab) && typeof startGrab === "function") r.grab.forEach(startGrab); // (grab.js)
  if (r && typeof takeJobs === "function") takeJobs(r); // downloads to hand back, cookies to send (takeover.js)
  return !!r;
}
async function config() {
  if (!appCfg) appCfg = (await B.storage.local.get({ appCfg: null })).appCfg;
  return appCfg || { skipSites: [], keys: { zipline: "alt", browser: "ctrlshift" } };
}
// Check in every 30 seconds while the app is there, so Zipline knows it can ask for fresh links. When
// it isn't, less and less often (2, 5, then every 10 minutes): a browser without Zipline isn't woken
// twice a minute for nothing. A check that works, or opening the popup, starts over at 30 seconds.
// (An alarm, not a timer: the worker sleeps in between, and its state is in storage.session.)
const PING_MINS = [0.5, 2, 5, 10];
let appUp = null; // the last check's answer (null: none since the worker started)
async function pingNow(fresh) {
  const ok = await ping();
  let fails = 0;
  try { fails = ok || fresh ? 0 : Math.min((await B.storage.session.get({ pingFails: 0 })).pingFails + 1, PING_MINS.length - 1); } catch {}
  appUp = ok;
  B.storage.session.set({ pingFails: fails, appUp: ok }).catch(() => {});
  B.alarms.create("zl-ping", { delayInMinutes: PING_MINS[fails] });
  if (typeof listenTakeover === "function") listenTakeover(); // (takeover.js)
  if (ok && typeof syncPrefs === "function") syncPrefs(); // (prefs.js)
  return ok;
}
pingNow();
B.storage.local.get({ appCfg: null }).then(s => { appCfg = appCfg || s.appCfg; useAppList(appCfg); }).catch(() => {});
B.alarms.onAlarm.addListener(a => { if (a.name === "zl-ping") pingNow(); });
let lastCfgPing = 0;
B.tabs.onActivated.addListener(() => { if (Date.now() - lastCfgPing > 30000) { lastCfgPing = Date.now(); pingNow(); } }); // (cheap while Zipline is away: a look at its port)

// siteListed: does the link's site match one of the patterns (example.com, *.example.com, example.com/path)?
function siteListed(list, url) {
  let u;
  try { u = new URL(url); } catch { return false; }
  const h = u.hostname.toLowerCase();
  return (list || []).some(rule => {
    let r = String(rule).trim().toLowerCase().replace(/^[a-z]+:\/\//, ""), path = "";
    const i = r.indexOf("/");
    if (i >= 0) { path = r.slice(i).replace(/\/+$/, ""); r = r.slice(0, i); }
    r = r.replace(/^\*\./, "").replace(/:\d+$/, "");
    if (!r || !(h === r || h.endsWith("." + r))) return false;
    return !path || u.pathname === path || u.pathname.startsWith(path + "/");
  });
}

// Links the user sent to the browser on purpose (hotkey), and downloads handed back to it: takeover
// leaves them alone. Kept in storage.session too, so a restarted worker still knows.
const bypass = new Map();
const bypassed = url => { const t = bypass.get(url); return t && Date.now() - t < 15000; };
function addBypass(url) {
  bypass.set(url, Date.now());
  for (const [u, t] of bypass) if (Date.now() - t > 15000) bypass.delete(u);
  B.storage.session.set({ bypass: Object.fromEntries(bypass) }).catch(() => {});
}
const bypassLoaded = B.storage.session.get({ bypass: {} }).then(s => { for (const [u, t] of Object.entries(s.bypass)) if (!bypass.has(u)) bypass.set(u, t); }).catch(() => {});

// ---- The right cookies for the right window
// A download's cookies are in the cookie store of the window it came from (a private window's, a
// Firefox container's), and cookies a site set while embedded in another page (CHIPS, Firefox's
// isolation) are in that page's partition. ctx says where: {store, top (the page's site), private}.
const FIREFOX = typeof B.runtime.getBrowserInfo === "function";
async function ctxOfTab(tab) {
  const ctx = { private: !!(tab && tab.incognito), top: siteOf(tab && tab.url) };
  if (tab && tab.cookieStoreId) ctx.store = tab.cookieStoreId; // (Firefox: containers, private windows)
  else if (tab && tab.incognito) {
    try { ctx.store = ((await B.cookies.getAllCookieStores()).find(s => s.tabIds.includes(tab.id)) || {}).id || "1"; } catch { ctx.store = "1"; }
  }
  return ctx;
}
function ctxOfItem(item) {
  const ctx = { private: !!item.incognito, top: siteOf(item.referrer || "") };
  if (item.cookieStoreId) ctx.store = item.cookieStoreId;
  else if (item.incognito) ctx.store = "1"; // (Chrome's private store)
  return ctx;
}
// siteOf: a page's scheme and registrable domain ("https://example.co.uk"), near enough without the
// public suffix list (a wrong guess only means a partition isn't found)
function siteOf(u) {
  let x;
  try { x = new URL(u); } catch { return ""; }
  if (!/^https?:$/.test(x.protocol)) return "";
  const l = x.hostname.split(".");
  if (l.length <= 2 || /^[\d.]+$/.test(x.hostname) || x.hostname.includes(":")) return x.protocol + "//" + x.hostname;
  const keep = l[l.length - 1].length === 2 && /^(co|com|net|org|gov|edu|ac|or|ne|go)$/.test(l[l.length - 2]) ? 3 : 2;
  return x.protocol + "//" + l.slice(-keep).join(".");
}

async function cookieHeader(url, ctx = {}) {
  const q = { url };
  if (ctx.store) q.storeId = ctx.store;
  const get = async more => {
    try { return await B.cookies.getAll({ ...q, ...more }); } catch (e) { // Firefox with first-party isolation wants to know whose
      if (/firstPartyDomain/i.test(String(e && e.message))) try { return await B.cookies.getAll({ ...q, ...more, firstPartyDomain: (ctx.top || siteOf(url)).replace(/^\w+:\/\//, "") }); } catch {}
      return [];
    }
  };
  const list = await get({});
  if (ctx.top) { // cookies set in a frame on that page (partitioned)
    for (const c of await get({ partitionKey: { topLevelSite: ctx.top } })) if (!list.some(x => x.name === c.name && x.path === c.path && x.domain === c.domain)) list.push(c);
  }
  return list.map(c => `${c.name}=${c.value}`).join("; ");
}

async function request(url, referer = "", filename = "", title = "", ctx = {}) {
  return { url, referer, filename, title, cookie: await cookieHeader(url, ctx), userAgent: navigator.userAgent, private: !!ctx.private };
}

// sendRes: {ok} or {ok: false, error}. The app can turn a link down (a protected site) with a message. The user
// sent the link (Download with Zipline), so it may start Zipline.
async function sendRes(url, referer = "", filename = "", title = "", ctx = {}) {
  const r = await call("add", await request(url, referer, filename, title, ctx), 5000, { start: true });
  return r && r.ok !== false ? { ok: true } : { ok: false, error: (r && r.error) || "Couldn't reach Zipline" };
}
const send = async (...a) => (await sendRes(...a)).ok;

const MENUS = [
  { id: "zl-link", title: "Download with Zipline", contexts: ["link"] },
  { id: "zl-media", title: "Download this media with Zipline", contexts: ["image", "video", "audio"] },
  { id: "zl-all", title: "Download all links with Zipline…", contexts: ["page", "frame", "selection"] },
  { id: "zl-scan", title: "Scan this site with Zipline…", contexts: ["page", "frame"] },
];
async function setupMenus() {
  try { await B.contextMenus.removeAll(); } catch {}
  for (const m of MENUS) { try { B.contextMenus.create(m, () => void B.runtime.lastError); } catch {} }
}
B.runtime.onInstalled.addListener(setupMenus);
B.runtime.onStartup.addListener(setupMenus);

B.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === "zl-all") {
    const r = await sendPageLinks(tab, info.frameId);
    if (!r.ok) notifyBadge("off");
    return;
  }
  if (info.menuItemId === "zl-scan") {
    if (!(await scanSite(tab)).ok) notifyBadge("off");
    return;
  }
  const url = info.linkUrl || info.srcUrl;
  if (!url || !/^(https?|ftps?|magnet):/i.test(url)) return;
  if (!(await pingNow())) { notifyBadge("off"); return; }
  if (!(await send(url, info.pageUrl || tab?.url || "", "", tab?.title || "", await ctxOfTab(tab)))) notifyBadge("off"); // (the app turned it down: a protected site)
});

// ---- Video detection ---------------------------------------------------------------------
// Watches responses for video/audio files and stream manifests (HLS .m3u8, DASH .mpd) so the popup and the on-page
// button can offer them. Pieces of a stream (.ts/.m4s chunks, byte-range parts) are ignored.
// One stream is one entry: the master playlist (or MPD) together with the media playlists, renditions and parts it
// lists, so the app's Add dialog gets the master and picks the quality itself. For each entry we keep what the
// player's own request carried (Referer, Origin, User-Agent, whether it had cookies); the popup and the on-page
// button both send exactly that.

const MEDIA_EXT = /\.(mp4|m4v|webm|mkv|mov|flv|avi|wmv|ogv|3gp|mp3|m4a|aac|ogg|oga|opus|flac|wav)$/i;
const CHUNK_EXT = /\.(ts|m4s|cmfv|cmfa|aac\d*|fmp4)$/i;
const CHUNK_URL = /\.(ts|m4s|cmfv|cmfa|aac\d*|fmp4)(?:[?#]|$)/i;
const CHUNK_QS = /[?&](bytestart|byteend|range|start_byte|end_byte)=/i;
const CHUNK_TYPE = /^video\/(mp2t|iso\.segment)$/;
const HLS_TYPE = /mpegurl/;
const DASH_TYPE = /dash\+xml|vnd\.mpeg\.dash\.mpd/;
// types that could be anything: a playlist is only recognised by reading the start of it
const AMBIGUOUS = /^(text\/plain|application\/octet-stream|binary\/octet-stream)$/;
const NOT_A_PLAYLIST = /\.(jpe?g|png|gif|webp|avif|svg|ico|css|m?js|json|map|woff2?|ttf|otf|eot|wasm|html?|xml|txt|csv|zip|gz|pdf|bin)$/i;
// an address that could be a playlist; only those get read when the reply's type says nothing (reading repeats a
// request, so it isn't done for every script's plain-text answer)
const PLAYLIST_WORDS = /m3u8?|mpd|playlist|manifest|master|hls|dash|stream|vod|video|media/i;
const REQ_TYPES = ["media", "xmlhttprequest", "other", "object"];
const MAX_PER_TAB = 30;
const MAX_SNIFF = 2 << 20; // how much of a playlist is read
const SNIFFS_PER_PAGE = 20; // reads of "might be a playlist" replies, per page load
const sleep = ms => new Promise(r => setTimeout(r, ms));

const mediaKey = tabId => "m:" + tabId;
const isStream = e => e.kind === "hls" || e.kind === "dash";

async function mediaFor(tabId) {
  const k = mediaKey(tabId);
  return (await B.storage.session.get(k))[k] || [];
}

let writeChain = Promise.resolve();
function updateMedia(tabId, change) {
  // serialize read-modify-write so concurrent responses don't drop entries
  writeChain = writeChain.then(async () => {
    const list = await mediaFor(tabId);
    change(list);
    await B.storage.session.set({ [mediaKey(tabId)]: list });
    B.action.setBadgeBackgroundColor({ tabId, color: "#1a6cf0" });
    B.action.setBadgeText({ tabId, text: list.length ? String(list.length) : "" });
  }).catch(() => {});
  return writeChain;
}

// covers: does entry e already include x (the same address, or a playlist / rendition / part e lists)?
// A listed file also counts when the player asks for it with another query (a fresh token). Parts are matched
// by folder, but only for plain files (never for another manifest).
const bare = u => u.split(/[?#]/)[0];
function covers(e, x) {
  if (e.url === x.url || (e.children || []).includes(x.url)) return true;
  if (!isStream(x) && (e.prefixes || []).some(p => x.url.startsWith(p))) return true;
  const b = bare(x.url);
  return /\.(m3u8|mpd|mp4|m4v|m4a|webm|mp3)$/i.test(b) && (e.children || []).some(c => bare(c) === b);
}
const union = (a, b, max) => [...new Set([...(a || []), ...(b || [])])].slice(0, max);

// fold: x belongs to stream e; take what it knows (a media playlist knows the length, whether it is live and its parts)
function fold(e, x) {
  if (!e.duration && x.duration) e.duration = x.duration;
  if (x.live) e.live = true;
  if (!e.hdr && x.hdr) e.hdr = x.hdr;
  e.children = union(e.children, x.children, 200);
  e.prefixes = union(e.prefixes, x.prefixes, 8);
  e.quals = union(e.quals, x.quals, 12).sort((a, b) => b - a);
  e.seen = Date.now();
}

function addMedia(tabId, item) {
  return updateMedia(tabId, list => {
    const same = list.find(x => x.url === item.url);
    if (same) {
      if (item.size > same.size) same.size = item.size;
      if (same.unparsed && !item.unparsed) { // read properly this time
        Object.assign(same, { kind: item.kind, children: item.children, prefixes: item.prefixes, quals: item.quals, duration: item.duration, live: item.live, unparsed: false });
      } else if (item.unparsed) same.tries = (same.tries || 0) + 1;
      absorb(list, same);
      return;
    }
    const owner = list.find(x => isStream(x) && covers(x, item));
    if (owner) return fold(owner, item);
    list.push(item);
    absorb(list, item);
    while (list.length > MAX_PER_TAB) list.shift();
  });
}

// absorb: entries seen before their stream's master (or the files of a DASH stream) belong to e now
function absorb(list, e) {
  if (!isStream(e)) return;
  for (let j = list.length - 1; j >= 0; j--) {
    if (list[j] !== e && covers(e, list[j])) { fold(e, list[j]); list.splice(j, 1); }
  }
}

const sniffs = new Map(); // tab -> reads of unclear replies so far
const pageCache = new Map(); // tab -> the page's title (see pageInfo)
function clearMedia(tabId) {
  B.storage.session.remove(mediaKey(tabId));
  B.action.setBadgeText({ tabId, text: "" }).catch(() => {});
  sniffs.delete(tabId);
  pageCache.delete(tabId);
}

function header(headers, name) {
  const h = (headers || []).find(x => x.name.toLowerCase() === name);
  return h ? h.value : "";
}

// ---- what the player's own request carried
const sentHdr = new Map(); // request id -> headers, until the reply arrives
(function watchSent() {
  const keep = d => {
    if (d.tabId < 0 || CHUNK_URL.test(d.url)) return;
    const h = d.requestHeaders;
    sentHdr.set(d.requestId, { referer: header(h, "referer"), origin: header(h, "origin"), ua: header(h, "user-agent"), cookie: !!header(h, "cookie") });
    if (sentHdr.size > 300) sentHdr.delete(sentHdr.keys().next().value);
  };
  const filter = { urls: ["http://*/*", "https://*/*"], types: REQ_TYPES };
  // Chrome only shows Referer, Origin and Cookie with "extraHeaders"; Firefox doesn't know that word
  try { B.webRequest.onBeforeSendHeaders.addListener(keep, filter, ["requestHeaders", "extraHeaders"]); }
  catch { B.webRequest.onBeforeSendHeaders.addListener(keep, filter, ["requestHeaders"]); }
})();

// ---- reading a playlist
// Chrome's service worker can't see a reply's body, so the playlist is fetched once more, with the site's cookies.
// A script can't set Referer or Origin on a fetch, so a short-lived rule (declarativeNetRequest) gives that one
// request the player's. Firefox can read the reply as it passes (filterResponseData, needs webRequestBlocking and, in Firefox 110 and later, webRequestFilterResponse), so
// the player's own request is the one we read there.
let ruleSeq = 1000;
async function withHeaders(url, hdr, run) {
  const dnr = B.declarativeNetRequest;
  const set = [["Referer", hdr && hdr.referer], ["Origin", hdr && hdr.origin]].filter(h => h[1]);
  if (!dnr || !dnr.updateSessionRules || !set.length || /[*^|]/.test(url)) return run();
  const id = ruleSeq++;
  if (ruleSeq > 1999) ruleSeq = 1000;
  try {
    await dnr.updateSessionRules({ removeRuleIds: [id], addRules: [{ id, priority: 1,
      action: { type: "modifyHeaders", requestHeaders: set.map(([header, value]) => ({ header, operation: "set", value })) },
      condition: { urlFilter: "|" + url + "|", resourceTypes: ["xmlhttprequest"], initiatorDomains: [B.runtime.id] } }] });
  } catch { return run(); }
  try { return await run(); } finally { dnr.updateSessionRules({ removeRuleIds: [id] }).catch(() => {}); }
}

function fetchText(url, hdr) {
  return withHeaders(url, hdr, async () => {
    const c = new AbortController(), t = setTimeout(() => c.abort(), 7000);
    try {
      const r = await fetch(url, { credentials: hdr && hdr.cookie === false ? "omit" : "include", signal: c.signal });
      if (!r.ok || !r.body) return null;
      const rd = r.body.getReader(), parts = [];
      for (let n = 0; n < MAX_SNIFF;) {
        const { done, value } = await rd.read();
        if (done) break;
        parts.push(value); n += value.length;
      }
      rd.cancel().catch(() => {});
      return await new Blob(parts).text();
    } catch { return null; } finally { clearTimeout(t); }
  });
}

const bodyOf = new Map(); // request id -> promise of the reply's text (Firefox)
(function watchBodies() {
  if (typeof B.webRequest.filterResponseData !== "function") return;
  try {
    B.webRequest.onHeadersReceived.addListener(d => {
      if (d.tabId < 0 || d.statusCode !== 200 || d.method !== "GET") return;
      let f;
      try { f = B.webRequest.filterResponseData(d.requestId); } catch { return; } // (not allowed: the playlist is fetched instead)
      const dec = new TextDecoder();
      let text = "", n = 0;
      bodyOf.set(d.requestId, new Promise(done => {
        f.ondata = e => {
          f.write(e.data); // (the page gets the reply untouched)
          try { if (n < MAX_SNIFF) { text += dec.decode(e.data, { stream: true }); n += e.data.byteLength; } } catch {}
        };
        f.onstop = () => { try { f.disconnect(); } catch {} done(text); };
        f.onerror = () => done(null);
      }));
      if (bodyOf.size > 50) bodyOf.delete(bodyOf.keys().next().value);
    }, { urls: ["*://*/*.m3u8*", "*://*/*.mpd*"], types: ["xmlhttprequest", "media", "other"] }, ["blocking", "responseHeaders"]);
  } catch {} // no webRequestBlocking: playlists are fetched instead
})();

async function bodyFor(d, hdr) {
  await null; // (the listener above has run by now)
  const p = bodyOf.get(d.requestId);
  bodyOf.delete(d.requestId);
  if (p) {
    const t = await Promise.race([p, sleep(5000).then(() => null)]);
    if (t) return t;
  }
  return fetchText(d.url, hdr);
}

// ---- understanding a playlist
const isoDur = s => {
  const m = /^P(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$/.exec(s || "");
  return m ? (+m[1] || 0) * 86400 + (+m[2] || 0) * 3600 + (+m[3] || 0) * 60 + (+m[4] || 0) : 0;
};
const pathOf = u => { try { return new URL(u).pathname; } catch { return ""; } };
const folderOf = u => u.split(/[?#]/)[0].replace(/[^/]*$/, "");
const heightsOf = set => [...set].sort((a, b) => b - a);

// parseManifest: null when the text isn't a playlist at all
function parseManifest(text, base) {
  const head = text.replace(/^﻿/, "").trimStart();
  if (head.startsWith("#EXTM3U")) return parseM3U8(head, base);
  if (/<MPD[\s>]/.test(head.slice(0, 4096))) return parseMPD(text, base);
  return null;
}

function parseM3U8(text, base) {
  const abs = u => { try { return new URL(u, base).href.split("#")[0]; } catch { return ""; } };
  const uriOf = l => { const m = /URI="([^"]*)"/i.exec(l); return m ? abs(m[1]) : ""; };
  const kids = new Set(), parts = new Set(), heights = new Set();
  let master = false, next = false, dur = 0, ended = false;
  for (const raw of text.split(/\r?\n/)) {
    const l = raw.trim();
    if (!l) continue;
    if (l[0] !== "#") { // a variant (after EXT-X-STREAM-INF) or a piece of the video
      const u = abs(l);
      if (u) (next ? kids : parts).add(u);
      next = false;
    } else if (l.startsWith("#EXT-X-STREAM-INF")) {
      master = next = true;
      const h = /RESOLUTION=\d+x(\d+)/i.exec(l);
      if (h) heights.add(+h[1]);
    } else if (l.startsWith("#EXT-X-MEDIA:") || l.startsWith("#EXT-X-I-FRAME-STREAM-INF")) { // audio, subtitle and other renditions
      const u = uriOf(l);
      if (u) kids.add(u);
    } else if (l.startsWith("#EXT-X-MAP:")) { const u = uriOf(l); if (u) parts.add(u); }
    else if (l.startsWith("#EXTINF:")) dur += parseFloat(l.slice(8)) || 0;
    else if (l.startsWith("#EXT-X-ENDLIST") || /^#EXT-X-PLAYLIST-TYPE:\s*VOD/i.test(l)) ended = true;
  }
  const out = { kind: "hls", master, children: [...kids].slice(0, 200), prefixes: [], quals: heightsOf(heights), duration: 0, live: false };
  if (!master) {
    out.live = !ended;
    out.duration = ended ? Math.round(dur) : 0;
    // parts that look like plain video files (a byte-range stream is one .mp4) would be listed as files of their own
    const files = [...parts].filter(u => MEDIA_EXT.test(pathOf(u)));
    if (files.length) { out.children = files.slice(0, 50); out.prefixes = [...new Set(files.map(folderOf))].slice(0, 3); }
  }
  return out;
}

function parseMPD(text, base) {
  const abs = (u, to = base) => { try { return new URL(u, to).href.split("#")[0]; } catch { return ""; } };
  const attr = (s, n) => { const m = new RegExp("\\b" + n + '="([^"]*)"').exec(s); return m ? m[1] : ""; };
  const tag = (/<MPD\b[^>]*>/.exec(text) || [""])[0];
  const heights = new Set(), kids = new Set(), prefixes = new Set();
  for (const m of text.matchAll(/<(?:Representation|AdaptationSet)\b[^>]*>/g)) { const h = +attr(m[0], "height"); if (h > 0) heights.add(h); }
  // single-file streams list their files in BaseURL; segment templates name theirs (the files seen are folded in too)
  const bases = [...text.matchAll(/<BaseURL[^>]*>([^<]+)<\/BaseURL>/g)].map(m => m[1].trim()).filter(Boolean);
  const top = bases.length && bases[0].endsWith("/") ? abs(bases[0]) : base;
  bases.forEach((b, i) => { const u = abs(b, i && top !== base ? top : base); if (u && MEDIA_EXT.test(pathOf(u))) kids.add(u); });
  for (const m of text.matchAll(/\b(?:media|initialization)="([^"]+)"/g)) {
    if (!MEDIA_EXT.test(m[1].split("$").pop().split("?")[0])) continue; // (.m4s pieces are ignored anyway)
    const p = m[1].split("$")[0];
    prefixes.add(p ? abs(p, top) : folderOf(top));
  }
  return { kind: "dash", master: true, children: [...kids].slice(0, 50), prefixes: [...prefixes].slice(0, 6), quals: heightsOf(heights),
    duration: Math.round(isoDur(attr(tag, "mediaPresentationDuration"))), live: attr(tag, "type") === "dynamic" };
}

// ---- seeing a stream: read it once, then add it (or fold it into the stream it belongs to)
const reading = new Set(), notPlaylist = new Set();
async function seeManifest(d, hint, type) {
  const url = d.url.split("#")[0], key = d.tabId + " " + url;
  const hdr = sentHdr.get(d.requestId);
  sentHdr.delete(d.requestId);
  if (reading.has(key) || notPlaylist.has(key)) return;
  reading.add(key);
  try {
    const known = (await mediaFor(d.tabId)).find(x => covers(x, { url, kind: "hls" }));
    let sniffForLength = false;
    if (known) {
      if (known.url === url) { if (!(known.unparsed && (known.tries || 0) < 3)) return; } // seen already (a live playlist is asked for again and again)
      else if (known.duration || known.durTried || known.kind === "dash") return;
      else sniffForLength = true; // one of its playlists: read it once for the length
    }
    if (!hint && !sniffForLength) {
      if ((sniffs.get(d.tabId) || 0) >= SNIFFS_PER_PAGE) return;
      sniffs.set(d.tabId, (sniffs.get(d.tabId) || 0) + 1);
    }
    if (sniffForLength) updateMedia(d.tabId, l => { const e = l.find(x => x.url === known.url); if (e) e.durTried = true; });
    const body = await bodyFor(d, hdr);
    const info = body == null ? null : parseManifest(body, url);
    if (!info && (body != null || !hint)) { // not a playlist after all (or an unclear reply that couldn't be read): don't look again
      if (notPlaylist.size > 500) notPlaylist.clear();
      notPlaylist.add(key);
      return;
    }
    await addMedia(d.tabId, {
      url, kind: info ? info.kind : hint, size: -1, type, name: safeName(pathOf(url)), seen: Date.now(), hdr,
      master: !!(info && info.master), children: info ? info.children : [], prefixes: info ? info.prefixes : [], quals: info ? info.quals : [],
      duration: info ? info.duration : 0, live: info ? info.live : false, unparsed: !info,
    });
  } finally { reading.delete(key); }
}

function safeName(path) {
  try { return decodeURIComponent(path.split("/").pop() || ""); } catch { return path.split("/").pop() || ""; }
}

B.webRequest.onHeadersReceived.addListener(details => {
  const sc = details.statusCode;
  if (details.tabId < 0 || sc >= 400 || (sc >= 300 && sc !== 304) || details.method !== "GET") return;
  const url = details.url;
  if (!/^https?:/i.test(url) || CHUNK_QS.test(url)) return;
  if (isBlockedHost(hostOf(url)) || isBlockedHost(hostOf(details.initiator || details.originUrl || details.documentUrl || ""))) return;
  const path = pathOf(url);
  if (!path || CHUNK_EXT.test(path)) return;
  const type = header(details.responseHeaders, "content-type").toLowerCase().split(";")[0].trim();
  if (CHUNK_TYPE.test(type)) return;
  const len = +header(details.responseHeaders, "content-length") || 0;
  const total = +((header(details.responseHeaders, "content-range").split("/")[1]) || 0) || len;
  // by the reply's type; by the address when the type is no help (anything but a web page or picture)
  const hlsT = HLS_TYPE.test(type), dashT = DASH_TYPE.test(type);
  const byPath = !(hlsT || dashT) && /\.(m3u8|mpd)$/i.test(path) && !/^(text\/html|image\/)/.test(type);
  if (hlsT || dashT || byPath) {
    const dash = dashT || (!hlsT && /\.mpd$/i.test(path));
    seeManifest(details, dash ? "dash" : "hls", type).catch(() => {});
    return;
  }
  // a reply that could be a playlist: read the start of it
  if (AMBIGUOUS.test(type) && PLAYLIST_WORDS.test(path + new URL(url).search) && !NOT_A_PLAYLIST.test(path) && !MEDIA_EXT.test(path) && total <= MAX_SNIFF) {
    seeManifest(details, "", type).catch(() => {});
    return;
  }
  const media = /^(video|audio)\//.test(type) || MEDIA_EXT.test(path);
  if (!media) return;
  if (total > 0 && total < 300 * 1024) return; // sound effects, previews
  const hdr = sentHdr.get(details.requestId);
  sentHdr.delete(details.requestId);
  addMedia(details.tabId, {
    url: url.split("#")[0], kind: type.startsWith("audio/") ? "audio" : "video", size: total, type, name: safeName(path), seen: Date.now(), hdr,
  });
}, { urls: ["http://*/*", "https://*/*"], types: REQ_TYPES }, ["responseHeaders"]);

B.tabs.onUpdated.addListener((tabId, info) => { if (info.status === "loading" && info.url) clearMedia(tabId); });
B.tabs.onRemoved.addListener(tabId => clearMedia(tabId));

// ---- naming what was found
// cleanTitle drops the site's name from a page title: "Clip - YouTube", "Clip | Vimeo", "Clip – Site Name".
// A trailing part is the site when it matches the site's own name (og:site_name, or its address); after a "|" any
// short tail is taken for one. After a dash it must match, so "Lecture 3 - Intro to Algebra" stays as it is.
function cleanTitle(raw, host = "", site = "") {
  let t = String(raw || "").replace(/[\u0000-\u001f\u007f]+/g, " ").replace(/\s+/g, " ").trim();
  const norm = s => s.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, "");
  const names = new Set([norm(site)]);
  const labels = String(host).toLowerCase().replace(/^www\./, "").split(".");
  names.add(norm(labels.join("")));
  labels.pop(); // (the ending: .com, .lk)
  if (labels.length > 1 && /^(co|com|org|net|gov|edu|ac)$/.test(labels[labels.length - 1])) labels.pop();
  for (const l of labels) if (!/^(www|m|web|player|embed|cdn|video|videos|watch|stream|media)$/.test(l)) names.add(norm(l));
  names.delete("");
  const isSite = seg => {
    const n = norm(seg);
    return n.length > 1 && [...names].some(x => n === x || (x.length >= 3 && (n.startsWith(x) || n.endsWith(x))) || (x.length >= 5 && n.includes(x)) || (n.length >= 4 && x.includes(n)));
  };
  const lead = /^(\S.*?)\s+(\||[-–—·•]|::|«|»)\s+(\S.*)$/s.exec(t); // "Site | Title"
  if (lead && isSite(lead[1]) && !isSite(lead[3])) t = lead[3];
  for (let i = 0; i < 2; i++) {
    const m = /^(.*\S)\s+(\||[-–—·•]|::|«|»)\s+(\S.*)$/s.exec(t);
    if (!m) break;
    const [, head, sep, tail] = m;
    // (a short tail after "|" is only taken for a site name once: "Episode 4 | Show | Site" keeps "Show")
    if (isSite(tail) || (i === 0 && sep === "|" && tail.length <= 30 && tail.split(" ").length <= 4 && head.length >= 4)) t = head; else break;
  }
  if (isSite(t) && names.has(norm(t))) return ""; // just the site's name
  return t.slice(0, 120);
}

// readPage runs inside the page: what it calls its video, and how long a playing one is
function readPage() {
  const meta = s => ((document.querySelector(s) || {}).content || "").trim();
  let dur = 0;
  for (const v of document.querySelectorAll("video")) if (isFinite(v.duration) && v.duration > dur) dur = v.duration;
  return { og: meta('meta[property="og:title"], meta[name="og:title"]'), tw: meta('meta[name="twitter:title"], meta[property="twitter:title"]'),
    site: meta('meta[property="og:site_name"]'), doc: document.title || "", dur };
}

// pageInfo: the page's title (og:title, twitter:title, then the document's, cleaned) and a playing video's length
async function pageInfo(tabId) {
  const c = pageCache.get(tabId);
  if (c && Date.now() - c.t < 3000) return c.info;
  const info = { title: "", dur: 0 };
  let tab = null;
  try { tab = await B.tabs.get(tabId); } catch {}
  const host = hostOf(tab && tab.url);
  let top = null;
  try {
    const res = await B.scripting.executeScript({ target: { tabId, allFrames: true }, func: readPage });
    for (const r of res || []) if (r.result) { if (r.frameId === 0) top = r.result; info.dur = Math.max(info.dur, r.result.dur || 0); }
  } catch {}
  info.title = cleanTitle((top && (top.og || top.tw || top.doc)) || (tab && tab.title) || "", host, top ? top.site : "");
  pageCache.set(tabId, { t: Date.now(), info });
  return info;
}

const GENERIC_NAME = /^(index|master|playlist|manifest|stream|main|hls|dash|video|chunklist|media|prog_index|playlist_dvr)$/i;
function streamName(e) {
  const n = (e.name || "").replace(/\.(m3u8|mpd)$/i, "");
  return n && !GENERIC_NAME.test(n) ? n : "Video stream";
}

// mediaList: this tab's entries, newest first, with the names and lengths the popup and the button show.
// src: the address the page's own <video> plays, offered too when no entry covers it.
async function mediaList(tabId, src) {
  const list = await mediaFor(tabId);
  const page = await pageInfo(tabId);
  const out = list.slice().reverse().map(e => ({
    url: e.url, kind: e.kind, size: e.size, name: e.name, live: !!e.live, quals: e.quals || [],
    title: isStream(e) ? (page.title || streamName(e)) : (e.name || (e.kind === "audio" ? "Audio" : "Video file")),
    duration: e.duration || (list.length === 1 ? page.dur : 0) || 0,
  }));
  if (src && /^https?:/i.test(src) && !isBlockedHost(hostOf(src)) && !list.some(e => covers(e, { url: src.split("#")[0], kind: "video" }))) {
    const kind = /\.m3u8(\?|$)/i.test(src) ? "hls" : /\.mpd(\?|$)/i.test(src) ? "dash" : "video";
    out.unshift({ url: src.split("#")[0], kind, size: -1, name: "", live: false, quals: [], title: page.title || "This video", duration: page.dur });
  }
  return out;
}

// sendMedia hands an entry to the app with the headers the player's own request had, whichever button was clicked.
// When the address is one of a stream's playlists, the stream's master is sent: the Add dialog picks the quality.
async function sendMedia(tabId, url, frameUrl) {
  let tab = null;
  try { tab = await B.tabs.get(tabId); } catch {}
  if (!(await pingNow())) return { ok: false, error: "Zipline isn't running" };
  const e = (await mediaFor(tabId)).find(x => covers(x, { url: url.split("#")[0], kind: "video" }));
  const target = e ? e.url : url;
  if (isBlockedHost(hostOf(target))) return { ok: false, error: blockedMsg() };
  const h = e && e.hdr; // (none when the request wasn't seen: the page it came from stands in)
  const page = await pageInfo(tabId), ctx = await ctxOfTab(tab);
  const r = await call("add", {
    url: target, referer: h ? h.referer : (frameUrl || tab?.url || ""), origin: h ? h.origin : "", userAgent: (h && h.ua) || navigator.userAgent,
    cookie: h && !h.cookie ? "" : await cookieHeader(target, ctx), title: page.title, private: ctx.private,
  }, 5000);
  return r && r.ok !== false ? { ok: true } : { ok: false, error: (r && r.error) || "Couldn't reach Zipline" };
}

// ---- Taking over browser downloads: takeover.js ------------------------------------------

// Hotkey + click on a link (links.js): send it to Zipline. If the app isn't running, the
// browser downloads it instead so the click isn't lost.
async function keyLink(msg, sender) {
  const referer = msg.referer || sender.tab?.url || "";
  if (await pingNow() && await send(msg.url, referer, "", sender.tab?.title || "", await ctxOfTab(sender.tab))) return { ok: true };
  notifyBadge("off");
  try { await B.downloads.download({ url: msg.url }); } catch {}
  return { ok: false };
}

function notifyBadge(state) {
  B.action.setBadgeText({ text: state === "off" ? "!" : "" });
  B.action.setBadgeBackgroundColor({ color: "#d93a3a" });
  if (state === "off") setTimeout(() => B.action.setBadgeText({ text: "" }), 4000);
}

B.runtime.onMessage.addListener((msg, sender, reply) => {
  switch (msg.type) {
    case "ping": pingNow(true).then(ok => reply({ ok })); return true; // (the popup: check again soon)
    case "magnet": (async () => {
      if (!(await pingNow())) return reply({ ok: false });
      reply({ ok: !!(await call("add", { url: msg.url, referer: msg.referer || "", title: sender.tab?.title || "", private: !!sender.tab?.incognito }, 5000)) });
    })(); return true;
    case "send": (async () => {
      const tab = msg.tabId != null ? await B.tabs.get(msg.tabId).catch(() => null) : null;
      reply(await sendRes(msg.url, msg.referer || "", "", msg.title || "", await ctxOfTab(tab)));
    })(); return true;
    case "media-list": {
      const tabId = msg.tabId ?? sender.tab?.id;
      if (tabId == null) { reply({ items: [] }); return; }
      mediaList(tabId, msg.src).then(items => reply({ items }), () => reply({ items: [] })); return true;
    }
    case "send-media": {
      const tabId = msg.tabId ?? sender.tab?.id;
      sendMedia(tabId, msg.url, msg.frameUrl || sender.url).then(reply); return true;
    }
    case "is-blocked": reply({ blocked: isBlockedHost(msg.host), message: blockedMsg() }); return;
    case "key-link": keyLink(msg, sender).then(reply); return true;
    case "bypass": addBypass(msg.url); reply({ ok: true }); return;
    case "page-links": {
      B.tabs.get(msg.tabId).then(tab => sendPageLinks(tab)).then(reply, e => reply({ ok: false, error: String(e && e.message || e) }));
      return true;
    }
    case "scan-site": {
      B.tabs.get(msg.tabId).then(tab => scanSite(tab)).then(reply, e => reply({ ok: false, error: String(e && e.message || e) }));
      return true;
    }
  }
});

// ---- "Download all links on this page" -------------------------------------------------
// Collects every link, image and media source on the page (all frames) and hands the list
// to Zipline, which opens a picker. No cookies go along: when you press Download there, the app
// asks for those of the chosen files' sites only (a cookie job, takeover.js).

function collectPageLinks() {
  const out = [], seen = new Set();
  const add = (u, text) => {
    try {
      const url = new URL(u, document.baseURI);
      if (!/^(https?|ftps?):$/.test(url.protocol)) return;
      url.hash = "";
      const k = url.href;
      if (seen.has(k) || out.length >= 5000) return;
      seen.add(k);
      out.push({ url: k, text: (text || "").replace(/\s+/g, " ").trim().slice(0, 140) });
    } catch {}
  };
  const sel = getSelection && getSelection();
  const inSel = el => !sel || sel.isCollapsed || sel.containsNode(el, true);
  document.querySelectorAll("a[href], area[href]").forEach(a => inSel(a) && add(a.href, a.textContent || a.title || a.getAttribute("aria-label")));
  document.querySelectorAll("img").forEach(i => inSel(i) && add(i.currentSrc || i.src, i.alt));
  document.querySelectorAll("video, audio").forEach(v => {
    if (!inSel(v)) return;
    if (v.currentSrc && !v.currentSrc.startsWith("blob:")) add(v.currentSrc, document.title);
    v.querySelectorAll("source[src]").forEach(s => add(s.src, document.title));
  });
  return out;
}

async function sendPageLinks(tab, frameId) {
  if (!tab || !/^https?:/i.test(tab.url || "")) return { ok: false, error: "Open a web page first" };
  if (!(await pingNow())) return { ok: false, error: "Zipline isn't running" };
  let results;
  try {
    const target = frameId ? { tabId: tab.id, frameIds: [frameId] } : { tabId: tab.id, allFrames: true };
    results = await B.scripting.executeScript({ target, func: collectPageLinks });
  } catch (e) {
    return { ok: false, error: "This page doesn't allow extensions to read it" };
  }
  const seen = new Set(), links = [];
  for (const r of results || []) for (const l of r.result || []) if (!seen.has(l.url)) { seen.add(l.url); links.push(l); }
  if (!links.length) return { ok: false, error: "No links found on this page" };
  const ctx = await ctxOfTab(tab); // (where the cookies are, for when the app asks)
  const r = await call("links", { page: tab.url, title: tab.title || "", links, userAgent: navigator.userAgent, ctx, private: ctx.private }, 8000, { start: true });
  if (r && r.ok && typeof waitJobs === "function") waitJobs(); // (the picker may ask for cookies soon)
  return r && r.ok ? { ok: true, count: links.length } : { ok: false, error: (r && r.error) || "Couldn't reach Zipline" };
}

// Site grabber helpers ("Scan this site", pages the app asks the browser to open): grab.js. Chrome loads it
// here; Firefox lists it before this file in its manifest.
try { importScripts("grab.js"); } catch {}

// ---- Fresh links for expired downloads ----------------------------------------------------
// When a download's link runs out, Zipline asks (in its ping reply) for the page it came from.
// The page opens in a background tab. Links on it to the same file, and any download the page
// starts by itself, go back to Zipline, which checks it's the same file (size and name) and
// carries on from where it stopped. The tab closes once it's found, or after 45 seconds (an alarm:
// the worker may sleep or restart meanwhile; what's in progress is kept in storage.session).

const refreshing = new Map(); // job id -> state
const baseName = u => { try { return decodeURIComponent(new URL(u).pathname.split("/").pop() || "").toLowerCase(); } catch { return ""; } };
const plainName = n => String(n || "").toLowerCase().replace(/ \(\d+\)(?=\.[^.]*$|$)/, "");
const refreshFor = tabId => { for (const st of refreshing.values()) if (st.tabId === tabId) return st; return null; };
const originOf = u => { try { return new URL(u).origin; } catch { return ""; } };

function refreshState(job, tabId = null, sent = [], urls = []) {
  const st = { job, tabId, sent: new Set(sent), urls: new Set(urls), done: false };
  st.finish = matched => {
    if (st.done) return;
    st.done = true;
    refreshing.delete(job.id);
    B.alarms.clear("zl-refresh:" + job.id).catch(() => {});
    saveRefresh(true);
    if (st.tabId != null) B.tabs.remove(st.tabId).catch(() => {});
    if (!matched) call("refresh", { id: job.id, done: true }, 5000);
  };
  return st;
}
let refreshTimer = 0;
function saveRefresh(now) {
  clearTimeout(refreshTimer);
  const save = () => B.storage.session.set({ refresh: [...refreshing.values()].map(st => ({ job: st.job, tabId: st.tabId, sent: [...st.sent], urls: [...st.urls].slice(-100) })) }).catch(() => {});
  if (now) save(); else refreshTimer = setTimeout(save, 300);
}
const refreshLoaded = B.storage.session.get({ refresh: [] }).then(s => {
  for (const x of s.refresh) if (x && x.job && !refreshing.has(x.job.id)) refreshing.set(x.job.id, refreshState(x.job, x.tabId, x.sent, x.urls));
}).catch(() => {});
B.alarms.onAlarm.addListener(async a => {
  if (!a.name.startsWith("zl-refresh:")) return;
  await refreshLoaded;
  const st = refreshing.get(a.name.slice(11));
  if (st) st.finish(false);
});

async function startRefresh(job) {
  await refreshLoaded;
  if (!job || !job.id || refreshing.has(job.id) || !/^https?:/i.test(job.page || "")) return;
  const st = refreshState(job);
  refreshing.set(job.id, st);
  B.alarms.create("zl-refresh:" + job.id, { when: Date.now() + 45000 });
  try { st.tabId = (await B.tabs.create({ url: job.page, active: false })).id; } catch { st.finish(false); }
  saveRefresh(true);
}

async function offer(st, urls, referer) {
  if (st.done) return;
  const fresh = [...new Set(urls)].filter(u => /^(https?|ftps?):/i.test(u) && !st.sent.has(u)).slice(0, 12);
  if (!fresh.length) return;
  fresh.forEach(u => st.sent.add(u));
  saveRefresh();
  const links = [];
  for (const u of fresh) links.push({ url: u, referer: referer || st.job.page, cookie: await cookieHeader(u) });
  const r = await call("refresh", { id: st.job.id, links, userAgent: navigator.userAgent }, 24000); // (under the 30 s an extension's request may take)
  if (r && (r.matched || r.gone)) st.finish(true);
}

// links on the page that may be the file: same name first, then anything that says "download"
async function scanPage(st) {
  if (st.done || st.tabId == null) return;
  let results = [];
  try { results = await B.scripting.executeScript({ target: { tabId: st.tabId, allFrames: true }, func: collectPageLinks }); } catch { return; }
  const want = plainName(st.job.name), named = [], maybe = [];
  for (const r of results || []) for (const l of r.result || []) {
    if (plainName(baseName(l.url)) === want || (l.text || "").toLowerCase().includes(want)) named.push(l.url);
    else if (/download|get[-_ ]?file|mirror/i.test(l.url + " " + (l.text || ""))) maybe.push(l.url);
  }
  await offer(st, named.concat(maybe.slice(0, 8)));
}

B.tabs.onUpdated.addListener((tabId, info) => {
  const st = info.status === "complete" && refreshFor(tabId);
  if (!st) return;
  setTimeout(() => scanPage(st), 1500);
  setTimeout(() => scanPage(st), 7000); // pages that show the link after a moment
});

// a reply in that tab that is the file (or looks like it)
B.webRequest.onHeadersReceived.addListener(details => {
  const st = refreshFor(details.tabId);
  if (!st) return;
  st.urls.add(details.url); // everything this tab fetched: only its own downloads are claimed
  saveRefresh();
  if (details.statusCode >= 400 || details.method !== "GET") return;
  const cd = header(details.responseHeaders, "content-disposition").toLowerCase();
  const len = +header(details.responseHeaders, "content-length") || 0;
  const total = +((header(details.responseHeaders, "content-range").split("/")[1]) || 0) || len;
  if (cd.startsWith("attachment") || (st.job.size > 0 && total === st.job.size) || plainName(baseName(details.url)) === plainName(st.job.name)) {
    offer(st, [details.url]);
  }
}, { urls: ["http://*/*", "https://*/*"] }, ["responseHeaders"]);

// a download the refresh tab started by itself: it's for Zipline, not a second copy in the
// browser. Only that tab's own downloads are claimed, never others from the same site.
function refreshClaims(item) {
  if (!refreshing.size) return false;
  for (const st of refreshing.values()) {
    if (st.urls.has(item.url) || st.urls.has(item.finalUrl || "")) {
      B.downloads.cancel(item.id).catch(() => {}).then(() => B.downloads.erase({ id: item.id }).catch(() => {}));
      offer(st, [item.finalUrl || item.url], item.referrer);
      return true;
    }
  }
  return false;
}

// Taking over downloads, handing them back, cookie jobs (takeover.js); the settings kept in the app
// and the keyboard shortcuts (prefs.js). Chrome loads them here; Firefox lists them after this file.
try { importScripts("takeover.js", "prefs.js"); } catch {}
