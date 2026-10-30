// Zipline site grabber, the browser's part (loaded by background.js: importScripts in Chrome, listed before
// it in the Firefox manifest, so everything here shares its globals - call, ping, hostOf, cookieHeader, collectPageLinks).
//  1. Pages that build their links with JavaScript: the app asks (in its ping reply) for a page to be opened in a
//     background tab. Once its script has run, the links on it (all frames) go back to Zipline, which adds them to the scan.
//     No cookies go with them: when you pick files to download, the app asks for those of their sites only (takeover.js).
//     What's in progress is kept in storage.session and the 75-second limit is an alarm, so a worker that
//     restarts meanwhile carries on (and closes the tab).
//  2. "Scan this site": the page and the browser's cookies for the site go to Zipline, which opens the site grabber on it.
const GB = globalThis.browser ?? globalThis.chrome;
const grabbing = new Map(); // ask id -> state

function grabState(ask, tabId = null, links = [], queued = false) {
  const st = { ask, tabId, done: false, queued, timers: [], links: new Map(links.map(l => [l.url, l])) };
  st.finish = async error => {
    if (st.done) return;
    st.done = true;
    st.timers.forEach(clearTimeout);
    grabbing.delete(ask.id);
    GB.alarms.clear("zl-grab:" + ask.id).catch(() => {});
    saveGrabs();
    if (st.tabId != null) GB.tabs.remove(st.tabId).catch(() => {});
    const links = [...st.links.values()].slice(0, 5000);
    const r = await call("grab", { id: ask.id, jobId: ask.jobId, links, error: error || (links.length ? "" : "empty"), userAgent: navigator.userAgent }, 20000);
    if (r && r.ok && typeof waitJobs === "function") waitJobs(); // (picking files may ask for cookies)
  };
  return st;
}
function saveGrabs() {
  GB.storage.session.set({ grabs: [...grabbing.values()].map(st => ({ ask: st.ask, tabId: st.tabId, queued: st.queued, links: [...st.links.values()].slice(0, 2000) })) }).catch(() => {});
}
// a restarted worker: carry on with what was in progress (a page already loaded is read now)
const grabsLoaded = GB.storage.session.get({ grabs: [] }).then(s => {
  for (const x of s.grabs) {
    if (!x || !x.ask || grabbing.has(x.ask.id)) continue;
    const st = grabState(x.ask, x.tabId, x.links, x.queued);
    grabbing.set(x.ask.id, st);
    if (st.queued) st.timers.push(setTimeout(async () => { await collectGrab(st); st.finish(st.links.size ? "" : "empty"); }, 1000));
  }
}).catch(() => {});
GB.alarms.onAlarm.addListener(async a => {
  if (!a.name.startsWith("zl-grab:")) return;
  await grabsLoaded;
  const st = grabbing.get(a.name.slice(8));
  if (st) st.finish(st.links.size ? "" : "timeout");
});

async function startGrab(ask) {
  await grabsLoaded;
  if (!ask || !ask.id || grabbing.has(ask.id) || !/^https?:/i.test(ask.page || "")) return;
  const st = grabState(ask);
  grabbing.set(ask.id, st);
  GB.alarms.create("zl-grab:" + ask.id, { when: Date.now() + 75000 });
  try { st.tabId = (await GB.tabs.create({ url: ask.page, active: false })).id; } catch { return st.finish("blocked"); }
  saveGrabs();
}

async function collectGrab(st) {
  let results = [];
  try {
    results = await GB.scripting.executeScript({ target: { tabId: st.tabId, allFrames: true }, func: collectPageLinks });
  } catch { return false; } // the page doesn't allow extensions to read it
  for (const r of results || []) for (const l of r.result || []) if (!st.links.has(l.url)) st.links.set(l.url, l);
  saveGrabs();
  return true;
}

// Script-built pages show their links a moment after loading: read twice, 2.5 and 6 seconds after it.
GB.tabs.onUpdated.addListener((tabId, info) => {
  if (info.status !== "complete") return;
  for (const st of grabbing.values()) {
    if (st.tabId !== tabId || st.queued || st.done) continue;
    st.queued = true;
    saveGrabs();
    st.timers.push(setTimeout(async () => {
      if (!(await collectGrab(st)) && !st.links.size) return st.finish("blocked");
    }, 2500));
    st.timers.push(setTimeout(async () => {
      await collectGrab(st);
      st.finish(st.links.size ? "" : "empty");
    }, 6000));
  }
});

// "Scan this site": the page, and the browser's cookies for the site (the page's own host, and its www / bare form),
// from the window it's open in (a private window's or a container's).
async function scanSite(tab) {
  if (!tab || !/^https?:/i.test(tab.url || "")) return { ok: false, error: "Open a web page first" };
  if (!(await pingNow())) return { ok: false, error: "Zipline isn't running" };
  const u = new URL(tab.url), bare = u.hostname.replace(/^www\./i, "").toLowerCase(), ctx = await ctxOfTab(tab);
  const cookies = {};
  for (const h of new Set([hostOf(tab.url), bare, "www." + bare])) {
    const c = await cookieHeader(h === hostOf(tab.url) ? tab.url : `${u.protocol}//${h}/`, ctx);
    if (c) cookies[h] = c;
  }
  const r = await call("scan", { page: tab.url, title: tab.title || "", cookies, userAgent: navigator.userAgent }, 8000, { start: true });
  return r && r.ok ? { ok: true } : { ok: false, error: (r && r.error) || "Couldn't reach Zipline" };
}
