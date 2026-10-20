// Zipline: taking over the browser's own downloads. Loaded after background.js (importScripts in Chrome,
// listed after it in the Firefox manifest), so it uses its helpers: call, pingNow, appUp, settings,
// config, siteListed, bypassed, addBypass, refreshClaims, cookieHeader, ctxOfItem, header, hostOf.
//
// The browser's request is the only one. Zipline gets it as the browser made it (its exact headers,
// a form's body) with what the reply said (name, size, whether it can resume), so it doesn't ask the
// server first, and a link that works only once still works.
//  - Chrome: decided in downloads.onDeterminingFilename, before the browser's Save As dialog, and only
//    while the app answers (Aria2 Explorer's way: the listener is there only then, and it answers at
//    once when it leaves a download alone, so no download waits on Zipline when it isn't running).
//  - Firefox: as soon as the download appears it's paused, erased, then cancelled (Motrix's order).
// Closing Zipline's Add dialog hands the download back: the app sends a "give back" job and the
// browser downloads it after all (started by the extension, so it isn't taken again).

// ---- the browser's request, as it was sent
// Request id -> {url, method, headers, rb (a form's body as Chrome or Firefox saw it), body64, resp, at}.
// Redirects keep the id: the entry ends up describing the last hop, the one the download came from.
const sentReqs = new Map();
const MAX_FORM = 64 << 10; // a form bigger than this (a file upload) isn't repeated: the browser keeps it
const DL_FILTER = { urls: ["http://*/*", "https://*/*"], types: ["main_frame", "sub_frame", "other"] };
function reqOf(id) {
  let e = sentReqs.get(id);
  if (!e) {
    sentReqs.set(id, e = { at: Date.now() });
    if (sentReqs.size > 200) sentReqs.delete(sentReqs.keys().next().value);
  }
  return e;
}
B.webRequest.onBeforeRequest.addListener(d => {
  if (d.method !== "POST") return;
  const rb = d.requestBody || {}, raw = rb.raw || [];
  const e = reqOf(d.requestId);
  if (!d.requestBody || rb.error || raw.some(p => !p.bytes) || raw.reduce((n, p) => n + p.bytes.byteLength, 0) > MAX_FORM) e.rb = { tooBig: true }; // (a file in it, or not shown)
  else e.rb = { form: rb.formData || null, raw: raw.length ? raw.map(p => p.bytes) : null };
}, DL_FILTER, ["requestBody"]);
(function watchDlHeaders() {
  const keep = d => { const e = reqOf(d.requestId); e.url = d.url; e.method = d.method; e.headers = d.requestHeaders || []; };
  // Chrome only shows Cookie, Referer and Origin with "extraHeaders"; Firefox doesn't know that word
  try { B.webRequest.onBeforeSendHeaders.addListener(keep, DL_FILTER, ["requestHeaders", "extraHeaders"]); }
  catch { B.webRequest.onBeforeSendHeaders.addListener(keep, DL_FILTER, ["requestHeaders"]); }
})();
B.webRequest.onHeadersReceived.addListener(d => {
  const e = sentReqs.get(d.requestId);
  if (!e || (d.statusCode >= 300 && d.statusCode < 400)) return;
  const hd = n => header(d.responseHeaders, n);
  e.url = d.url;
  e.resp = { status: d.statusCode, type: hd("content-type"), len: +hd("content-length") || -1, range: hd("content-range"), ranges: /bytes/i.test(hd("accept-ranges")),
    etag: hd("etag"), lastMod: hd("last-modified"), disp: hd("content-disposition") };
  if (e.method === "POST") e.body64 = formBody(e);
  // a reply that will be a download: kept in storage.session until it's decided (the worker may restart)
  if (/^attachment/i.test(e.resp.disp) || (d.type !== "sub_frame" && e.resp.type && !/^(text\/html|application\/xhtml)/i.test(e.resp.type))) {
    const keepIt = { url: e.url, method: e.method, headers: e.headers, body64: e.body64, resp: e.resp, at: e.at };
    B.storage.session.get({ dlReqs: [] }).then(s => B.storage.session.set({ dlReqs: [keepIt, ...s.dlReqs.filter(x => Date.now() - x.at < 120000)].slice(0, 10) })).catch(() => {});
  }
}, DL_FILTER, ["responseHeaders"]);

// formBody: the form as it went out (base64), or null when it can't be made again
function formBody(e) {
  const rb = e.rb || {}, type = header(e.headers, "content-type");
  let bytes = null;
  if (rb.raw) {
    const all = new Uint8Array(rb.raw.reduce((n, b) => n + b.byteLength, 0));
    let at = 0;
    for (const b of rb.raw) { all.set(new Uint8Array(b), at); at += b.byteLength; }
    bytes = all;
  } else if (rb.form && /^application\/x-www-form-urlencoded/i.test(type)) {
    const q = new URLSearchParams();
    for (const [k, vs] of Object.entries(rb.form)) for (const v of vs) q.append(k, v);
    bytes = new TextEncoder().encode(q.toString());
  } else if (rb.form && /^multipart\/form-data/i.test(type)) { // (the browser read the fields out of it: put them back)
    const b = (/boundary=("?)([^";]+)\1/i.exec(type) || [])[2];
    if (!b) return null;
    let t = "";
    for (const [k, vs] of Object.entries(rb.form)) for (const v of vs) t += `--${b}\r\nContent-Disposition: form-data; name="${k.replace(/"/g, "%22")}"\r\n\r\n${v}\r\n`;
    bytes = new TextEncoder().encode(t + `--${b}--\r\n`);
  } else if (!rb.form && !rb.raw && !rb.tooBig) bytes = new Uint8Array(0); // an empty form
  if (!bytes || bytes.length > MAX_FORM) return null;
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(bin);
}

// sentFor: the request a download came from (the newest one for its address), if it was seen
async function sentFor(item) {
  const urls = [item.finalUrl, item.url].filter(Boolean);
  let best = null;
  for (const e of sentReqs.values()) if (urls.includes(e.url) && e.resp && (!best || e.at > best.at)) best = e;
  if (best) return best;
  try { return (await B.storage.session.get({ dlReqs: [] })).dlReqs.find(x => urls.includes(x.url)) || null; } catch { return null; }
}

// ---- deciding
// skipByType: leave web pages and similar to the browser. Judged by the file's own name or type,
// not the link's ending: download.php?id=… or get.aspx usually serve a real file.
const SCRIPT_EXT = /^(php\d?|asp|aspx|jsp|cgi|pl|do|action|ashx|py)$/i;
function skipByType(item, path, s) {
  const skip = String(s.skipTypes || "").toLowerCase().split(/\s+/).filter(Boolean);
  const nameExt = (((item.filename || "").split(/[\\/]/).pop() || "").match(/\.([a-z0-9]+)$/i) || [])[1] || "";
  const urlExt = path.includes(".") ? path.split(".").pop() : "";
  const ext = (nameExt || (SCRIPT_EXT.test(urlExt) ? "" : urlExt)).toLowerCase();
  if (ext && skip.includes(ext)) return true;
  return /^(text\/html|application\/xhtml\+xml)/i.test(item.mime || "");
}

const claimed = new Set(); // downloads a fresh-link tab started (background.js)
const ready = Promise.all([bypassLoaded, refreshLoaded, B.storage.session.get({ appUp: null }).then(s => { if (appUp === null) appUp = s.appUp; }).catch(() => {})]);

// decide: whether Zipline takes this download, and the request it came from. Quick: everything it
// reads is at hand, so a download Zipline leaves alone isn't held up.
async function decide(item) {
  await ready;
  if (claimed.has(item.id) || (item.byExtensionId && item.byExtensionId === B.runtime.id) || appUp !== true) return null; // (ours: handed back, or a link while the app was away)
  const url = item.finalUrl || item.url;
  if (!/^https?:/i.test(url) || bypassed(item.url) || bypassed(url)) return null;
  const s = await settings(), cfg = await config();
  let path = "";
  try { path = new URL(url).pathname.toLowerCase(); } catch { return null; }
  // .torrent files go to Zipline even when it doesn't take over other downloads (Options → Torrents in the app)
  const torrent = path.endsWith(".torrent") || /bittorrent/i.test(item.mime || "") || /\.torrent$/i.test(item.filename || "");
  if (!s.takeover && !(torrent && cfg.torrents !== false)) return null;
  if (!torrent && (skipByType(item, path, s) || (item.totalBytes > 0 && item.totalBytes < s.minKB * 1024))) return null;
  if (siteListed(cfg.skipSites, url) || siteListed(cfg.skipSites, item.url) || siteListed(cfg.skipSites, item.referrer || "")) return null;
  const req = await sentFor(item);
  if (req && req.method === "POST" && req.body64 == null) return null; // a form that can't be sent again (a file in it)
  if (req && req.method === "POST" && item.incognito && !FIREFOX) return null; // (it couldn't be handed back to the private window)
  return { torrent, req };
}

// handOver: the download goes to Zipline's Add dialog with the browser's own request
async function handOver(item, { torrent, req }) {
  const url = (req && req.url) || item.finalUrl || item.url, ctx = ctxOfItem(item);
  const name = (item.filename || "").split(/[\\/]/).pop();
  const headers = {};
  for (const x of (req && req.headers) || []) if (x.value != null) headers[x.name] = x.value;
  const post = req && req.method === "POST";
  const r = req && req.resp;
  const size = item.totalBytes > 0 ? item.totalBytes : r && r.len > 0 && r.status === 200 ? r.len : -1;
  const job = { url, method: post ? "POST" : "", body: post ? req.body64 : "", headers, filename: name, ctx };
  const res = await call("add", {
    url, referer: item.referrer || header(req && req.headers, "referer"), filename: name, title: "",
    cookie: header(req && req.headers, "cookie") || await cookieHeader(url, ctx), // (the request's own, when it was seen)
    userAgent: header(req && req.headers, "user-agent") || navigator.userAgent,
    headers, method: job.method, body: job.body || undefined, taken: true, private: ctx.private, ctx,
    known: r && !torrent ? { name, size, mime: item.mime || r.type.split(";")[0], finalUrl: url, resumable: r.ranges && size > 0, etag: r.etag, lastMod: r.lastMod } : undefined,
  }, 8000);
  if (!res || res.ok === false) return giveBack(job); // Zipline couldn't take it after all: the browser downloads it
  waitJobs(); // its Add dialog is open: a give-back may come
}

// Chrome: before the browser picks a name (and before its Save As dialog)
function determine(item, suggest) {
  let answered = false;
  const answer = () => { if (!answered) { answered = true; try { suggest(); } catch {} } };
  decide(item).then(async d => {
    if (!d) return answer();
    try { await B.downloads.cancel(item.id); } catch {}
    answer();
    B.downloads.erase({ id: item.id }).catch(() => {});
    handOver(item, d);
  }, answer);
  return true; // (suggest comes later: the decision reads settings)
}
// listenTakeover: the listener is there only while the app answers. Right after the worker starts it's
// there anyway, so the download that woke it is seen; it goes as soon as the stored answer says so.
function listenTakeover() {
  const ev = B.downloads.onDeterminingFilename;
  if (!ev) return;
  const want = appUp !== false;
  if (want && !ev.hasListener(determine)) ev.addListener(determine);
  if (!want && ev.hasListener(determine)) ev.removeListener(determine);
}
listenTakeover();
ready.then(listenTakeover);

B.downloads.onCreated.addListener(async item => {
  await refreshLoaded;
  if (refreshClaims(item)) { claimed.add(item.id); return; } // the page opened for a fresh link started it
  if (appUp === false && !item.byExtensionId) pingNow(); // the app may be back: the next download is taken again
  if (B.downloads.onDeterminingFilename) return; // (Chrome decides in determine)
  const d = await decide(item);
  if (!d) return;
  if (d.torrent && item.state === "complete") { // a small .torrent file is often done already
    try { await B.downloads.removeFile(item.id); } catch {}
  } else try { await B.downloads.pause(item.id); } catch {}
  try { await B.downloads.erase({ id: item.id }); } catch {}
  try { await B.downloads.cancel(item.id); } catch {}
  handOver(item, d);
});

// ---- handing a download back to the browser
// Headers a script may not set (the browser sends its own Cookie, Referer and User-Agent).
const NOT_SETTABLE = /^(accept-charset|accept-encoding|access-control-.*|connection|content-length|cookie2?|date|dnt|expect|host|keep-alive|origin|referer|set-cookie|te|trailer|transfer-encoding|upgrade|upgrade-insecure-requests|via|user-agent|range|if-.*|priority|authorization)$|^(proxy|sec)-/i;
const givenBack = new Set();
async function giveBack(job) {
  if (!job || !/^https?:/i.test(job.url || "") || (job.id && givenBack.has(job.id))) return;
  if (job.id) givenBack.add(job.id);
  const ctx = job.ctx || {}, post = job.method === "POST";
  addBypass(job.url);
  if (ctx.private && !FIREFOX) { // Chrome starts an extension's download in the normal window: open the address in a private one instead
    if (!post) openPrivate(job.url);
    return;
  }
  const opts = { url: job.url, conflictAction: "uniquify" };
  const name = String(job.filename || "").split(/[\\/]/).pop().replace(/^\.+/, "");
  if (name) opts.filename = name;
  const headers = Object.entries(job.headers || {}).filter(([k]) => !NOT_SETTABLE.test(k)).map(([name, value]) => ({ name, value: String(value) }));
  if (headers.length) opts.headers = headers;
  if (post) {
    opts.method = "POST";
    opts.body = new TextDecoder().decode(Uint8Array.from(atob(job.body || ""), c => c.charCodeAt(0)));
  }
  if (FIREFOX) {
    if (ctx.private) opts.incognito = true;
    if (ctx.store && !/^firefox-(default|private)$/.test(ctx.store)) opts.cookieStoreId = ctx.store; // (a container)
  }
  try { await B.downloads.download(opts); }
  catch { delete opts.filename; delete opts.headers; try { await B.downloads.download(opts); } catch {} } // (a name or header it wouldn't take)
}
async function openPrivate(url) {
  try {
    const w = (await B.windows.getAll({ windowTypes: ["normal"] })).find(x => x.incognito);
    if (w) await B.tabs.create({ windowId: w.id, url, active: false });
    else await B.windows.create({ url, incognito: true, focused: false });
  } catch {}
}

// ---- jobs from the app
// They come with each ping, and sooner while a dialog that may need the extension is open: then it
// waits on /api/ext/jobs, which answers as soon as there's one (each wait under 25 s, the most an
// extension's request may take; an extension call in between keeps the worker awake).
let jobsLoop = null;
function waitJobs() {
  if (jobsLoop) return jobsLoop;
  jobsLoop = (async () => {
    try {
      for (let i = 0; i < 100; i++) { // (half an hour at most)
        const r = await call("jobs", { wait: 20 }, 24000);
        if (!r) break;
        takeJobs(r, true);
        if (!r.wait) break;
        await B.storage.session.get("appUp");
      }
    } finally { jobsLoop = null; }
  })();
  return jobsLoop;
}
function takeJobs(r, looping) {
  if (Array.isArray(r.giveback)) r.giveback.forEach(giveBack);
  if (Array.isArray(r.cookies)) r.cookies.forEach(sendCookies);
  if (r.wait && !looping) waitJobs();
}
// sendCookies: the picker's Download asks for the cookies of the sites of the files you chose, only those
async function sendCookies(job) {
  const cookies = {};
  for (const u of (job.urls || []).slice(0, 40)) {
    const h = hostOf(u);
    if (h && !(h in cookies)) cookies[h] = await cookieHeader(u, job.ctx || {});
  }
  call("cookies", { job: job.id, cookies }, 5000);
}
