"use strict";
/* Zipline Download Manager — UI */

const APP_VERSION = "__ZL_VERSION__"; // (the app fills it in as it serves this file: server.go)

// ------------------------------------------------------------------ helpers
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const h = (html) => { const tpl = document.createElement("template"); tpl.innerHTML = html.trim(); return tpl.content.firstElementChild; };

// The window is signed in by its session cookie (session.go); X-Zipline marks its own requests,
// which a form on another site can't send.
const ZH = { "X-Zipline": "1" };
async function api(path, body) {
  const r = await fetch("/api/" + path, {
    method: body === undefined ? "GET" : "POST",
    headers: { ...ZH, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

const isHLS = d => d.kind === "hls";
const urlName = u => { try { const p = new URL(u).pathname.split("/").filter(Boolean).pop(); return p ? decodeURIComponent(p) : new URL(u).host; } catch { return u; } };
const dname = d => d.name || urlName(d.url);
const pct = d => {
  if (d.status === "completed") return 100;
  if (d.live) return 0; // a recording has no end to measure against
  if (isHLS(d)) return d.prog != null ? Math.min(100, d.prog * 100) : d.parts > 0 ? Math.min(100, d.partsDone / d.parts * 100) : 0;
  return d.size > 0 ? Math.min(100, d.done / d.size * 100) : 0;
};
const etaOf = d => {
  const total = isHLS(d) ? d.est : d.size;
  return total > 0 && d.speed > 0 ? Math.max(0, total - d.done) / d.speed : -1;
};
const sizeText = d => d.status !== "completed" && isHLS(d) ? (d.est > 0 ? t("size.approx", { size: fmtSize(d.est) }) : "") : (d.size >= 0 ? fmtSize(d.size) : "");
const fmtDur = s => { s = Math.round(s || 0); const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), x = s % 60; return (h ? h + ":" + String(m).padStart(2, "0") : m) + ":" + String(x).padStart(2, "0"); };
// hmSecs reads a length typed as h:mm (or just minutes) into seconds; -1 when it doesn't read.
const hmSecs = v => { const m = /^\s*(?:(\d{1,3}):)?(\d{1,4})\s*$/.exec(v); return !m || m[1] && +m[2] > 59 ? -1 : (+(m[1] || 0) * 60 + +m[2]) * 60; };

// NO_SHARE_WORD is what a finished torrent's row says when its files weren't there to share again.
const NO_SHARE_WORD = { missing: "st.completeMissing", parts: "st.completeMissing", changed: "st.completeChanged" };
// torrentStatus is a torrent's list status where it differs from a plain download's.
function torrentStatus(d) {
  const tr = d.torrent;
  if (d.status === "completed") {
    if (tr.seeding && d.scanState !== "threat") return t(tr.held ? "st.completeSharingPaused" : "st.completeSharing");
    const word = tr.noShare && NO_SHARE_WORD[tr.noShare.why];
    return word ? t(word) : d.metered ? t("st.completeShareLater") : null; // (metered: it carries on sharing on a normal connection)
  }
  if (d.status === "downloading" && tr.checking) return t("st.checkingFiles", { pct: fmtPct(tr.checkPct || 0, 0) });
  if (d.status !== "downloading" || d.stage || capHeld() || quotaHeld()) return null;
  if (tr.needMeta) return tr.metaPct ? t("st.gettingListPct", { pct: fmtPct(tr.metaPct, 0) }) : t("st.gettingList");
  return t("st.peers", { pct: fmtPct(pct(d), 2), peers: tr.peers || 0 });
}
// withPct puts how far a download got in front of a status word (when it got anywhere).
const withPct = (d, digits, word) => pct(d) > 0 ? t("st.withPct", { pct: fmtPct(pct(d), digits), word }) : word;
function statusText(d) {
  if (d.move) return t("st.moving", { pct: fmtPct(d.move.pct, 0) });
  if (d.torrent) { const x = torrentStatus(d); if (x) return x; }
  switch (d.status) {
    case "completed":
      if (d.stage) return t("st.completeChecking", { pct: fmtPct(d.stagePct || 0, 0) });
      if (d.check && d.check.state === "bad") return t("st.completeDamaged");
      return t(d.scanState === "scanning" || d.scanState === "waiting" ? "st.completeScanning" : d.scanState === "threat" ? "st.completeThreat" : d.check && d.check.state === "ok" ? "st.completeVerified" : "st.complete");
    case "error": {
      if (d.tryAgain) return t("st.errorRetrying", { wait: fmtWait(Math.max(0, d.tryAgain - Date.now()) / 1000) }); // its queue tries it again
      const w = t(d.link === "refreshing" ? "st.linkRefreshing" : d.link ? "st.linkExpired" : "st.error");
      return d.link ? withPct(d, 1, w) : w;
    }
    case "connecting": {
      if (!d.retryAt) return t("st.connecting");
      return t("st.retrying", { wait: fmtWait(Math.max(0, d.retryAt - Date.now()) / 1000) }); // every connection was lost: it tries again by itself (the bar shows how far it got)
    }
    case "queued": {
      const inRun = d.queue && S.queues.some(q => q.id === d.queue && q.running);
      const w = d.queue === "freehours" && S.plan && S.plan.on && !S.plan.in ? "st.waitFreeHours" : d.waiting ? "st.waitSlot"
        : inRun && S.plan && S.plan.metered ? "st.waitMetered" : inRun && quotaHeld() ? "st.waitQuota" : "st.queued";
      return withPct(d, 1, t(w));
    }
    case "paused":
      return withPct(d, 2, t(d.metered ? "st.pausedMetered" : "st.paused"));
    case "downloading":
      if (d.stage) return t("st.withPct", { pct: fmtPct(d.stagePct || 0, 0), word: d.stage });
      if (capHeld()) return withPct(d, 1, t("st.capReached"));
      if (quotaHeld()) return withPct(d, 1, t("st.quotaUsedUp"));
      if (d.retrying) return t("st.reconnecting");
      if (d.live) return t("st.recording", { time: fmtDur(d.rec) });
      return d.size > 0 || (isHLS(d) && d.parts > 0) ? fmtPct(pct(d), 2) : fmtSize(d.done);
  }
  return d.status;
}

// ------------------------------------------------------------------ icons (original line icons)
const I = {
  logo: `<svg width="20" height="20" viewBox="0 0 32 32"><rect width="32" height="32" rx="8" fill="#1a6cf0"/><path d="M17.5 5 9 18h6l-1.5 9L23 14h-6z" fill="#fff"/></svg>`,
  add: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#1a6cf0" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12" r="9.2" fill="#1a6cf01a"/><path d="M12 7.5v9M7.5 12h9"/></svg>`,
  resume: `<svg width="24" height="24" viewBox="0 0 24 24"><path d="M8 5.5v13a.8.8 0 0 0 1.2.7l10.3-6.5a.8.8 0 0 0 0-1.4L9.2 4.8A.8.8 0 0 0 8 5.5z" fill="#1f9d55"/></svg>`,
  stop: `<svg width="24" height="24" viewBox="0 0 24 24"><rect x="6" y="6" width="12" height="12" rx="2.2" fill="#e0533b"/></svg>`,
  stopAll: `<svg width="24" height="24" viewBox="0 0 24 24"><rect x="3.5" y="7" width="10" height="10" rx="2" fill="#e0533b" opacity=".55"/><rect x="10.5" y="7" width="10" height="10" rx="2" fill="#e0533b"/></svg>`,
  del: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#d93a3a" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M4.5 7h15M9.5 7V5h5v2M6.5 7l1 12.5h9l1-12.5M10.2 10.5v6M13.8 10.5v6"/></svg>`,
  delDone: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M4.5 7h11M8.5 7V5h4v2M6 7l1 12.5h7l1-12.5" stroke="#d93a3a"/><path d="m15.5 15 2.3 2.3 4.2-5" stroke="#1f9d55"/></svg>`,
  options: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="12" cy="12" r="3"/><path d="M12 2.8v2.4M12 18.8v2.4M2.8 12h2.4M18.8 12h2.4M5.5 5.5l1.7 1.7M16.8 16.8l1.7 1.7M5.5 18.5l1.7-1.7M16.8 7.2l1.7-1.7"/></svg>`,
  sched: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#8a5cf6" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12.5" r="8.3"/><path d="M12 8v4.7l3 2M9 2.8h6"/></svg>`,
  qStart: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 6h9M3.5 11h9M3.5 16h5" stroke="currentColor"/><path d="M13.5 12.5v8l7-4z" fill="#1f9d55" stroke="#1f9d55" stroke-linejoin="round"/></svg>`,
  qStop: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 6h9M3.5 11h9M3.5 16h5" stroke="currentColor"/><rect x="13.5" y="13" width="7" height="7" rx="1.3" fill="#e0533b" stroke="none"/></svg>`,
  limiter: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.8 17a8.5 8.5 0 1 1 16.4 0"/><path d="m12 15 3.8-5.2"/><circle cx="12" cy="15.2" r="1.4" fill="currentColor"/></svg>`,
  batch: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#1a6cf0" stroke-width="1.7" stroke-linecap="round"><rect x="3.5" y="3.5" width="11" height="11" rx="2"/><rect x="9.5" y="9.5" width="11" height="11" rx="2" fill="#1a6cf01a"/><path d="M15 12.8v5M12.5 15.3h5"/></svg>`,
  folderOpen: `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"><path d="M1.8 12.5V3.8a1 1 0 0 1 1-1h3.1l1.4 1.5h4.9a1 1 0 0 1 1 1v1.2"/><path d="M1.8 12.5 3.6 7.3a1 1 0 0 1 .95-.7h9.6a.7.7 0 0 1 .66.93l-1.6 4.5a.7.7 0 0 1-.66.47z"/></svg>`,
  listQ: `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="#8a5cf6" stroke-width="1.6" stroke-linecap="round"><path d="M2.5 4h11M2.5 8h11M2.5 12h7"/></svg>`,
  syncQ: `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="#169c8a" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M13 6.4A5.2 5.2 0 0 0 3.4 5.1M3 9.6a5.2 5.2 0 0 0 9.6 1.3"/><path d="M3.2 2.4v2.9h2.9M12.8 13.6v-2.9H9.9"/></svg>`,
  grab: `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.7" stroke-linecap="round"><circle cx="10.5" cy="10.5" r="7" stroke="#169c8a"/><path d="M3.5 10.5h14M10.5 3.5c-2.2 2.2-2.2 11.8 0 14M10.5 3.5c2.2 2.2 2.2 11.8 0 14" stroke="#169c8a" stroke-width="1.3"/><path d="M15.8 15.8 20.5 20.5" stroke="currentColor" stroke-width="2.2"/></svg>`,
  plus: `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M8 3.5v9M3.5 8h9"/></svg>`,
  more: `<svg width="24" height="24" viewBox="0 0 24 24" fill="currentColor"><circle cx="5.5" cy="12" r="1.8"/><circle cx="12" cy="12" r="1.8"/><circle cx="18.5" cy="12" r="1.8"/></svg>`,
  trash: `<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2.8 4.5h10.4M6.2 4.5V3h3.6v1.5M4.2 4.5l.7 8.5h6.2l.7-8.5"/></svg>`,
  play: `<svg width="12" height="12" viewBox="0 0 12 12"><path d="M3.2 1.9v8.2a.5.5 0 0 0 .75.43l6.6-4.1a.5.5 0 0 0 0-.86l-6.6-4.1a.5.5 0 0 0-.75.43z" fill="currentColor"/></svg>`,
  folder: c => `<svg width="16" height="16" viewBox="0 0 16 16"><path d="M1.5 4a1 1 0 0 1 1-1h3.6l1.5 1.5h5.9a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H2.5a1 1 0 0 1-1-1z" fill="${c}"/></svg>`,
};
const CAT = {
  Compressed: { c: "#c98a14", g: `<path d="M4 2.5h8a1 1 0 0 1 1 1v9a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-9a1 1 0 0 1 1-1z" fill="#c98a14"/><path d="M7.3 2.5h1.4v1.2H7.3zm0 2h1.4v1.2H7.3zm0 2h1.4v1.2H7.3z" fill="#fff"/><rect x="6.6" y="8.4" width="2.8" height="2.2" rx=".5" fill="#fff"/>` },
  Documents: { c: "#2f6fe0", g: `<path d="M4 1.5h5.5L13 5v8.5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-11a1 1 0 0 1 1-1z" fill="#2f6fe0"/><path d="M5.3 8h5.4M5.3 10h5.4M5.3 12h3.4" stroke="#fff" stroke-width="1"/>` },
  Music: { c: "#d6408a", g: `<rect x="2.5" y="2" width="11" height="12" rx="2" fill="#d6408a"/><path d="M7 11.2V5.3l4-1v5.3" stroke="#fff" stroke-width="1.1" fill="none"/><circle cx="6" cy="11.2" r="1.3" fill="#fff"/><circle cx="10" cy="9.8" r="1.3" fill="#fff"/>` },
  Programs: { c: "#5b6475", g: `<rect x="1.8" y="2.5" width="12.4" height="11" rx="1.6" fill="#5b6475"/><path d="M1.8 5.2h12.4" stroke="#fff" stroke-width=".9"/><path d="m5 8 2 1.5L5 11M8.5 11h2.5" stroke="#fff" stroke-width="1" fill="none"/>` },
  Video: { c: "#7a4fe0", g: `<rect x="1.8" y="3" width="12.4" height="10" rx="1.6" fill="#7a4fe0"/><path d="M6.6 5.8v4.4L10.3 8z" fill="#fff"/>` },
  Images: { c: "#169c8a", g: `<rect x="1.8" y="2.5" width="12.4" height="11" rx="1.6" fill="#169c8a"/><circle cx="5.6" cy="6" r="1.3" fill="#fff"/><path d="m3 12 3.5-3.5 2 2 2-2.5L13 11.5V12z" fill="#fff"/>` },
  General: { c: "#8a93a3", g: `<path d="M4 1.5h5.5L13 5v8.5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-11a1 1 0 0 1 1-1z" fill="#8a93a3"/><path d="M9.5 1.5V5H13" fill="#c8cdd6"/>` },
};
const catIcon = c => `<svg width="16" height="16" viewBox="0 0 16 16" style="flex:none">${(CAT[c] || CAT.General).g}</svg>`;

// ------------------------------------------------------------------ state
const S = {
  downloads: [], byId: new Map(), selected: new Set(), anchor: null,
  filter: "all", sort: { col: "added", dir: 1 }, search: "",
  settings: null, queueRunning: false, totalSpeed: 0, queues: [],
  lastSeq: null, handledPrompts: new Set(), promptBusy: false,
  progress: new Map(), // id -> dialog
  limitOn: false, limitKB: 0,
};
try { const st = JSON.parse(localStorage.getItem("zl.ui") || "{}"); if (st.sort) S.sort = st.sort; if (st.filter) S.filter = st.filter === "queue" ? "q:main" : st.filter; if (st.widths) COLW = st.widths; } catch {}
const URL_RE = /^(https?|ftps?):\/\/\S+$/i;
const LINK_RE = /^((https?|ftps?):\/\/\S+|magnet:\?\S+)$/i; // what the Add dialog takes (magnet links too)
const qName = id => (S.queues.find(q => q.id === id) || {}).name || "";
const qBusy = q => q.running || q.syncing;
function saveUI() { try { localStorage.setItem("zl.ui", JSON.stringify({ sort: S.sort, filter: S.filter, widths: COLW })); } catch {} }

// ------------------------------------------------------------------ menu bar
function menuItem(label, key, fn, opts = {}) { return { label, key, fn, ...opts }; }
const SEP = { sep: true };
function menuDefs() {
  const sl = sel(), one = sl.length === 1 ? sl[0] : null, none = sl.length === 0;
  return [
    [t("menu.file"), [
      menuItem(t("a.open"), "Enter", () => act("open"), { disabled: !one || one.status !== "completed" }),
      menuItem(t("a.openFolder"), "", () => act("folder"), { disabled: none }),
      menuItem(t("a.properties"), "Alt+Enter", () => propsDialog(one), { disabled: !one }),
      SEP,
      menuItem(t("a.removeFromList"), "Del", () => removeSel(false), { disabled: none }),
      menuItem(t("a.deleteFile"), "Shift+Del", () => removeSel(true), { disabled: none }),
    ]],
    [t("menu.downloads"), [
      menuItem(t("a.resume"), "", () => act("start"), { disabled: none || sl.every(d => d.status === "completed" && !canShareAgain(d)) }),
      menuItem(t("a.pause"), "", () => act("pause"), { disabled: !sl.some(d => ["downloading", "connecting", "queued"].includes(d.status)) }),
      menuItem(t("a.pauseAll"), "", () => pauseAll()),
      menuItem(t("a.resumeAll"), "", () => resumeAll()),
      menuItem(t("a.redownload"), "", () => redownload(sel()), { disabled: none }),
      SEP,
      menuItem(t("menu.startQueue"), "", () => queueAction("start"), { disabled: S.queues.every(qBusy) }),
      menuItem(t("menu.stopQueue"), "", () => queueAction("stop"), { disabled: !S.queues.some(qBusy) }),
      menuItem(t("menu.queues"), "", () => queueManager()),
      SEP,
      menuItem(t(S.limitOn ? "menu.limiterOff" : "menu.limiterOn"), "", () => toggleLimiter()),
      menuItem(t("a.removeCompleted"), "", () => removeCompleted()),
      menuItem(t("menu.options"), "", () => optionsDialog()),
    ]],
    [t("menu.view"), [
      menuItem(t("menu.toggleCategories"), "", () => { const s = $("#sidebar"); s.style.display = s.style.display === "none" ? "" : "none"; }),
      menuItem(t("menu.columns"), "", () => columnsDialog()),
      menuItem(t("menu.notifications"), "", () => notesDialog()),
      SEP,
      menuItem(t("menu.theme", { name: t("theme.system") }), "", () => setTheme("")),
      menuItem(t("menu.theme", { name: t("theme.light") }), "", () => setTheme("light")),
      menuItem(t("menu.theme", { name: t("theme.dark") }), "", () => setTheme("dark")),
    ]],
    [t("menu.tasks"), [
      menuItem(t("menu.addLink"), "Ctrl+N", () => addUrlDialog()),
      menuItem(t("menu.addBatch"), "Ctrl+B", () => batchDialog()),
      menuItem(t("menu.siteGrabber"), "Ctrl+G", () => grabberDialog()),
      menuItem(t("menu.pasteLink"), "Ctrl+V", () => pasteAdd()),
      menuItem(t("menu.openTorrent"), "Ctrl+O", () => openTorrentFile()),
      SEP,
      menuItem(t("menu.phone"), "", () => optionsDialog("phone")),
      SEP,
      menuItem(t("menu.import"), "", () => importDialog()),
      menuItem(t("menu.export"), "", () => exportDialog()),
      SEP,
      menuItem(t("menu.whenDone"), "", null, { sub: whenDoneItems() }),
      menuItem(t("menu.exit"), "", () => quitApp()),
    ]],
    [t("menu.help"), [
      menuItem(t("menu.integration"), "", () => optionsDialog("integration")),
      menuItem(t("menu.shortcuts"), "", () => shortcutsDialog()),
      menuItem(t("menu.cli"), "", () => cliDialog()),
      menuItem(t("menu.diagnostics"), "", () => diagDialog()),
      menuItem(t("menu.about"), "", () => aboutDialog()),
    ]],
  ];
}
function buildMenus() {
  const bar = $("#menubar");
  bar.innerHTML = "";
  bar.setAttribute("role", "menubar"); bar.setAttribute("aria-label", t("menu.bar"));
  menuDefs().forEach(([name], idx) => {
    const mi = h(`<div class="mi" role="none"><div class="mi-l" role="menuitem" tabindex="${idx ? -1 : 0}" aria-haspopup="menu" aria-expanded="false">${esc(name)}</div><div class="dropdown" role="menu" aria-label="${esc(name)}"></div></div>`);
    // items are rebuilt every time a menu opens so they reflect the current selection
    mi._fill = () => fillMenu($(".dropdown", mi), menuDefs()[idx][1]);
    mi.addEventListener("mousedown", e => {
      if (e.target.closest(".dropdown")) return;
      e.preventDefault(); // (openBarMenu moves the focus itself)
      const was = mi.classList.contains("open");
      noteMenuReturn();
      if (was) closeMenus(); else { closeMenus(true); openBarMenu(mi); }
    });
    mi.addEventListener("mouseenter", () => {
      if ($(".mi.open") && !mi.classList.contains("open")) { closeMenus(true); openBarMenu(mi); }
    });
    bar.appendChild(mi);
  });
}
// openBarMenu opens a menu bar menu, focusing its first or last item (from the keyboard) or its title.
function openBarMenu(mi, focus) {
  mi._fill(); mi.classList.add("open"); keepMenuOnScreen(mi);
  const l = $(".mi-l", mi);
  l.setAttribute("aria-expanded", "true"); setBarTab(l);
  if (focus) focusMenuItem($(".dropdown", mi), focus === "last" ? -1 : 0); else l.focus({ preventScroll: true });
}
// keepMenuOnScreen keeps a menu-bar menu inside the window: moved left when it would pass the right
// edge, and scrolling when it is taller than the space below the bar (small windows, big zoom).
function keepMenuOnScreen(mi) {
  const dd = $(".dropdown", mi);
  dd.style.left = ""; dd.style.maxHeight = "";
  let r = dd.getBoundingClientRect();
  dd.style.maxHeight = Math.max(80, innerHeight - r.top - 8) + "px";
  r = dd.getBoundingClientRect();
  const shift = Math.min(0, innerWidth - 6 - r.right), at = mi.getBoundingClientRect().left;
  if (shift) dd.style.left = Math.round(Math.max(shift, 6 - at)) + "px";
}
// placeMenu puts the floating menu (#ctx) at x, y, moved so all of it shows; one taller than the window scrolls.
function placeMenu(m, x, y) {
  m.style.maxHeight = innerHeight - 12 + "px";
  const r = m.getBoundingClientRect();
  m.style.left = Math.max(6, Math.min(x, innerWidth - r.width - 6)) + "px";
  m.style.top = Math.max(6, Math.min(y, innerHeight - r.height - 6)) + "px";
}
function fillMenu(el, items) {
  el.innerHTML = "";
  el.setAttribute("role", "menu");
  for (const it of items) {
    if (it.sep) { el.appendChild(h(`<div class="dd-sep" role="separator"></div>`)); continue; }
    const ck = items.some(x => "checked" in x) ? `<span class="ck" aria-hidden="true">${it.checked ? "✓" : ""}</span>` : "";
    const row = h(`<div class="dd-item${it.disabled ? " disabled" : ""}" role="menuitem${"checked" in it ? "checkbox" : ""}" tabindex="-1"${it.disabled ? ` aria-disabled="true"` : ""}${"checked" in it ? ` aria-checked="${!!it.checked}"` : ""}>${ck}<span class="lb">${asHtml(it.label)}</span><span class="k">${esc(it.sub ? "›" : it.key || "")}</span></div>`);
    row._it = it;
    if (it.sub) {
      row.setAttribute("aria-haspopup", "menu"); row.setAttribute("aria-expanded", "false");
      const open = () => openSubmenu(row, it.sub);
      row.addEventListener("mouseenter", open); row.addEventListener("mouseup", open);
    } else {
      row.addEventListener("mouseup", () => menuPick(row));
      if (el.id !== "ctx") row.addEventListener("mouseenter", closeSubmenu);
    }
    row.addEventListener("mouseenter", () => row.focus({ preventScroll: true })); // (one highlight for mouse and keys)
    el.appendChild(row);
  }
}
// closeMenus closes every menu; focus that was in one goes back where it was (stay: it doesn't).
function closeMenus(stay) {
  const inMenu = !stay && document.activeElement && document.activeElement.closest(".dropdown,#ctx");
  $$(".mi.open").forEach(m => { m.classList.remove("open"); $(".mi-l", m).setAttribute("aria-expanded", "false"); });
  $("#ctx").style.display = "none"; closeSubmenu();
  if (inMenu) restoreMenuFocus();
}
// openSubmenu shows a menu item's choices beside it (or below it when there's no room).
function openSubmenu(row, items) {
  const m = $("#ctx");
  closeSubmenu();
  fillMenu(m, items);
  m.dataset.sub = "1"; m.style.display = "block"; row.classList.add("sub-open"); row.setAttribute("aria-expanded", "true");
  const a = row.getBoundingClientRect(), r = m.getBoundingClientRect();
  let left = a.right + 2, top = a.top - 5;
  if (left + r.width > innerWidth - 6) left = a.left - r.width - 2;
  if (left < 6) { left = Math.max(6, innerWidth - r.width - 6); top = a.bottom + 2; }
  placeMenu(m, left, top);
}
function closeSubmenu() {
  const m = $("#ctx");
  $$(".dd-item.sub-open").forEach(x => { x.classList.remove("sub-open"); x.setAttribute("aria-expanded", "false"); });
  if (m.dataset.sub) { m.style.display = "none"; delete m.dataset.sub; }
}
document.addEventListener("mousedown", e => { if (!e.target.closest(".mi") && !e.target.closest("#ctx")) closeMenus(); });

// ---- menus from the keyboard (the WAI-ARIA menubar and menu patterns)
// Alt or F10 goes to the menu bar (and back); Left/Right move between its menus, Down/Up/Home/End
// within one, Right/Left into and out of a submenu, Enter or Space chooses, a letter jumps to the
// next item starting with it, and Escape goes back out. Focus returns to where it was.
let menuReturn = null;
function noteMenuReturn() { const a = document.activeElement; if (!a || !a.closest("#menubar,#ctx")) menuReturn = a; }
function restoreMenuFocus() {
  const r = menuReturn;
  if (r && r !== document.body && r.isConnected && r.getClientRects().length) r.focus({ preventScroll: true });
  else if (document.activeElement && document.activeElement.closest("#menubar,#ctx")) document.activeElement.blur();
}
const barLabels = () => $$("#menubar .mi-l");
function setBarTab(l) { barLabels().forEach(x => x.tabIndex = x === l ? 0 : -1); }
const menuRows = m => $$(":scope > .dd-item", m);
function focusMenuItem(m, i) { const L = menuRows(m); if (L.length) L[(i % L.length + L.length) % L.length].focus({ preventScroll: true }); }
function toggleMenubar() {
  if (document.activeElement && document.activeElement.closest("#menubar,#ctx")) { closeMenus(true); restoreMenuFocus(); return; }
  noteMenuReturn(); closeMenus(true);
  setBarTab(barLabels()[0]); barLabels()[0].focus(); // (File, as on the desktop)
}
// menuOpened: a pop-up menu (#ctx) is showing; it gets the keys (its first item when opened from the keyboard).
function menuOpened(m) { noteMenuReturn(); if (lastKb) focusMenuItem(m, 0); else m.focus({ preventScroll: true }); }
// menuPick chooses an item: a submenu opens, anything else closes the menus and runs.
function menuPick(row) {
  const it = row._it;
  if (!it || row.classList.contains("disabled")) return;
  if (it.sub) { openSubmenu(row, it.sub); focusMenuItem($("#ctx"), 0); return; }
  closeMenus(); it.fn();
}
// menuNav handles the keys every menu shares; false when it isn't one of them.
function menuNav(e, m, item) {
  const L = menuRows(m), i = L.indexOf(item), k = e.key;
  if (k === "ArrowDown") focusMenuItem(m, i + 1);
  else if (k === "ArrowUp") focusMenuItem(m, i < 0 ? -1 : i - 1);
  else if (k === "Home" || k === "PageUp") focusMenuItem(m, 0);
  else if (k === "End" || k === "PageDown") focusMenuItem(m, -1);
  else if ((k === "Enter" || k === " ") && item) menuPick(item);
  else if (k.length === 1 && k !== " " && !e.ctrlKey && !e.metaKey && !e.altKey) { // type-ahead
    const ch = k.toLowerCase();
    for (let n = 1; n <= L.length; n++) { const x = L[(i + n) % L.length]; if ($(".lb", x).textContent.trim().toLowerCase().startsWith(ch)) { x.focus(); break; } }
  } else return false;
  return true;
}
$("#menubar").addEventListener("keydown", e => {
  const mi = e.target.closest(".mi"); if (!mi) return;
  const bar = $$("#menubar > .mi"), i = bar.indexOf(mi), k = e.key, item = e.target.closest(".dd-item");
  const go = (j, open) => { const n = bar[(j + bar.length) % bar.length]; closeMenus(true); if (open) openBarMenu(n, "first"); else { setBarTab($(".mi-l", n)); $(".mi-l", n).focus(); } };
  if (item) { // in an open menu
    if (k === "ArrowRight") { if (item._it && item._it.sub) menuPick(item); else go(i + 1, true); }
    else if (k === "ArrowLeft") go(i - 1, true);
    else if (k === "Escape") { closeMenus(true); $(".mi-l", mi).focus(); }
    else if (k === "Tab") { closeMenus(true); restoreMenuFocus(); }
    else if (!menuNav(e, item.parentElement, item)) return;
  } else { // on the bar itself
    const open = mi.classList.contains("open");
    if (k === "ArrowRight" || k === "ArrowLeft") go(i + (k === "ArrowRight" ? 1 : -1), open);
    else if (k === "Home" || k === "End") go(k === "Home" ? 0 : -1, open);
    else if (k === "ArrowDown" || k === "Enter" || k === " ") openBarMenu(mi, "first");
    else if (k === "ArrowUp") openBarMenu(mi, "last");
    else if (k === "Escape") { closeMenus(true); restoreMenuFocus(); }
    else if (k === "Tab") { closeMenus(true); return; } // (Tab moves on as usual)
    else if (k.length === 1 && k !== " " && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const j = bar.findIndex((x, n) => n > i && x.textContent.trim().toLowerCase().startsWith(k.toLowerCase()));
      const j0 = j >= 0 ? j : bar.findIndex(x => x.textContent.trim().toLowerCase().startsWith(k.toLowerCase()));
      if (j0 < 0) return;
      go(j0, open);
    } else return;
  }
  e.preventDefault(); e.stopPropagation();
});
$("#ctx").tabIndex = -1;
$("#ctx").addEventListener("keydown", e => {
  const m = $("#ctx"), k = e.key, item = e.target.closest(".dd-item"), parent = m.dataset.sub ? $(".dd-item.sub-open") : null;
  if (parent && (k === "ArrowLeft" || k === "Escape")) { closeSubmenu(); parent.focus(); }
  else if (parent && k === "ArrowRight" && !(item && item._it && item._it.sub)) { // on to the next menu in the bar
    const bar = $$("#menubar > .mi"), mi = parent.closest(".mi");
    closeMenus(true); if (mi) openBarMenu(bar[(bar.indexOf(mi) + 1) % bar.length], "first");
  }
  else if (k === "Escape" || k === "Tab") { closeMenus(true); restoreMenuFocus(); }
  else if (!menuNav(e, m, item)) return;
  e.preventDefault(); e.stopPropagation();
});
function setTheme(theme) {
  if (theme) document.documentElement.dataset.theme = theme; else delete document.documentElement.dataset.theme;
  try { localStorage.setItem("zl.theme", theme); } catch {}
  if (S.settings && (S.settings.theme || "") !== theme) { S.settings.theme = theme; api("theme", { theme }).catch(() => {}); } // (the browser extension's menu follows it)
}
try { const theme = localStorage.getItem("zl.theme"); if (theme) document.documentElement.dataset.theme = theme; } catch {}

// ------------------------------------------------------------------ toolbar
// [id, label (a message ID), icon, what it does, priority]. The priority says how long a button stays on the bar as
// the window gets narrower: the lowest numbers go under "More" first (see fitToolbar).
const TB = [
  ["add", "a.addLink", I.add, () => addUrlDialog(), 100],
  ["resume", "a.resume", I.resume, () => act("start"), 90],
  ["stop", "a.pause", I.stop, () => act("pause"), 80],
  ["stopAll", "a.pauseAll", I.stopAll, () => pauseAll(), 50],
  ["del", "a.remove", I.del, () => removeSel(false), 70],
  ["delDone", "a.removeCompleted", I.delDone, () => removeCompleted(), 20],
  "|",
  ["options", "a.options", I.options, () => optionsDialog(), 60],
  ["sched", "a.scheduler", I.sched, () => queueManager(), 40],
  ["qStart", "a.startQueue", I.qStart, e => queueAction("start", e.currentTarget), 45],
  ["qStop", "a.stopQueue", I.qStop, e => queueAction("stop", e.currentTarget), 44],
  "|",
  ["limiter", "a.speedLimiter", I.limiter, () => toggleLimiter(), 30],
  ["batch", "a.batch", I.batch, () => batchDialog(), 25],
  ["grab", "a.siteGrabber", I.grab, () => grabberDialog(), 35],
];
function buildToolbar() {
  const tb = $("#toolbar");
  const cmds = h(`<div id="tb-cmds"></div>`), find = h(`<div id="tb-find"></div>`);
  tb.append(cmds, find);
  for (const x of TB) {
    if (x === "|") { cmds.appendChild(h(`<div class="tb-sep" aria-hidden="true"></div>`)); continue; }
    const b = h(`<button class="tb" id="tb-${x[0]}" title="${esc(t(x[1]))}" aria-label="${esc(t(x[1]))}" data-pri="${x[4]}">${x[2]}<span>${esc(t(x[1]))}</span></button>`);
    b.onclick = x[3]; b._tb = x;
    cmds.appendChild(b);
  }
  const more = h(`<button class="tb" id="tb-more" title="${te("tb.more")}" aria-label="${te("tb.moreCommands")}" aria-haspopup="menu" hidden>${I.more}<span>${te("tb.more")}</span></button>`);
  more.onclick = () => showMenuAt(more, moreItems(more));
  cmds.appendChild(more);
  const vs = h(`<select id="viewSel" class="inp" title="${te("tb.show")}" aria-label="${te("tb.show")}"></select>`);
  vs.onchange = () => { S.filter = vs.value; saveUI(); renderAll(); };
  find.appendChild(vs);
  const s = h(`<input id="search" placeholder="${te("tb.search")}" aria-label="${te("tb.search")}" spellcheck="false">`);
  s.oninput = () => { S.search = s.value.toLowerCase(); renderTable(); };
  find.appendChild(s);
  new ResizeObserver(fitToolbar).observe(tb);
  new ResizeObserver(fitToolbar).observe(find); // (the view picker's list arrives later and changes its width)
  document.fonts?.ready.then(fitToolbar);
  fitToolbar();
}
// fitToolbar keeps every button that fits and puts the rest under "More" (⋯), least important first,
// like a WinUI command bar. The search box only gets narrower; it never leaves.
function fitToolbar() {
  const tb = $("#toolbar"), cmds = $("#tb-cmds"), more = $("#tb-more");
  if (!cmds || !more) return;
  const btns = $$(".tb[data-pri]", cmds), leastFirst = [...btns].sort((a, b) => a.dataset.pri - b.dataset.pri);
  const seps = () => { // a divider shows only while it has a button on each side
    const kids = [...cmds.children].filter(x => x !== more);
    kids.forEach((x, i) => { if (x.classList.contains("tb-sep")) x.hidden = !(kids.slice(0, i).some(y => y.dataset.pri && !y.hidden) && kids.slice(i + 1).some(y => y.dataset.pri && !y.hidden)); });
  };
  const over = () => tb.scrollWidth > tb.clientWidth + 1 || cmds.scrollWidth > cmds.clientWidth + 1;
  const focus = document.activeElement;
  btns.forEach(b => b.hidden = false); more.hidden = true; seps();
  for (const b of leastFirst) {
    if (!over()) break;
    b.hidden = true; more.hidden = false; seps();
  }
  if (focus && focus.hidden && tb.contains(focus)) more.focus(); // (the button the keyboard was on moved under More)
}
// moreItems lists the buttons that don't fit, in toolbar order, as menu items.
function moreItems(anchor) {
  const items = []; let group = 0, last = 0;
  for (const el of $("#tb-cmds").children) {
    if (el.classList.contains("tb-sep")) { group++; continue; }
    if (!el.hidden || !el._tb) continue;
    const x = el._tb;
    if (items.length && group !== last) items.push(SEP);
    last = group;
    items.push(menuItem(t(x[1]), x[0] === "limiter" ? t(S.limitOn ? "tb.limiterState.on" : "tb.limiterState.off") : "", () => x[3]({ currentTarget: anchor }), { disabled: el.disabled }));
  }
  return items;
}
function updateToolbar() {
  const sl = sel();
  $("#tb-resume").disabled = !sl.some(d => d.status !== "completed" && !["downloading", "connecting"].includes(d.status) || canShareAgain(d));
  $("#tb-stop").disabled = !sl.some(d => ["downloading", "connecting", "queued"].includes(d.status));
  $("#tb-del").disabled = sl.length === 0;
  $("#tb-stopAll").disabled = !S.downloads.some(d => ["downloading", "connecting"].includes(d.status));
  $("#tb-delDone").disabled = !S.downloads.some(d => d.status === "completed");
  const tq = queueTarget(), tqq = S.queues.find(q => q.id === tq);
  $("#tb-qStart").disabled = tqq ? qBusy(tqq) : S.queues.every(qBusy);
  $("#tb-qStop").disabled = tqq ? !qBusy(tqq) : !S.queues.some(qBusy);
  $("#tb-qStart").title = tqq ? t("a.startNamed", { name: tqq.name }) : t("tb.startQueueAny");
  $("#tb-qStop").title = tqq ? t("a.stopNamed", { name: tqq.name }) : t("tb.stopQueueAny");
  $("#tb-limiter").classList.toggle("on", S.limitOn);
  $("#tb-limiter").setAttribute("aria-pressed", S.limitOn);
  $("#tb-limiter").title = (S.limitOn ? t("tb.limiterOn", { rate: fmtKBps(S.limitKB) }) : t("tb.limiterOff")) + "\n" + t("tb.limiterNote");
}

// ------------------------------------------------------------------ queues
function queueTarget() { return S.filter.startsWith("q:") ? S.filter.slice(2) : null; }
function showMenuAt(anchor, items) {
  const m = $("#ctx");
  delete m.dataset.sub;
  fillMenu(m, items);
  m.style.display = "block";
  const a = anchor.getBoundingClientRect(), r = m.getBoundingClientRect();
  const above = a.top - r.height - 4; // (from the status bar it opens upwards)
  placeMenu(m, a.left, a.bottom + 4 + r.height > innerHeight - 6 && above >= 6 ? above : a.bottom + 4);
  menuOpened(m);
}
// Start/stop the queue being viewed; otherwise the only candidate, or let the user pick.
function queueAction(action, anchor) {
  const go = id => action === "start" ? startQueue(id) : api("queue", { action, id });
  const target = queueTarget();
  if (target) return go(target);
  const qs = S.queues.filter(q => action === "start" ? !qBusy(q) : qBusy(q));
  if (qs.length === 1) return go(qs[0].id);
  if (!qs.length) return;
  const run = q => go(q.id);
  showMenuAt(anchor || $("#tb-qStart"), qs.map(q => menuItem(th(action === "start" ? "a.startNamed" : "a.stopNamed", { name: nm(q.name) }), q.sync ? t("queue.syncTag") : "", () => run(q))));
}

// ------------------------------------------------------------------ sidebar
const CATS = ["Compressed", "Documents", "Music", "Programs", "Video", "Images", "General"];
const catName = c => CATS.includes(c) ? t("cat." + c) : c; // (a category of the user's own keeps its name)
// mlFacts are the Add dialog's rows about a metalink after its type: its servers, checksums (the whole file's
// and one per piece), and the torrent and folders when it has them.
function mlFacts(fs) {
  const row = (label, v) => `<div>${label}</div><div class="wrap">${v}</div>`;
  const most = l => Math.max(0, ...l.map(f => f.mirrors)), tor = fs.filter(f => f.torrent);
  const dirs = [...new Set(fs.filter(f => f.name.includes("/")).map(f => f.name.slice(0, f.name.lastIndexOf("/") + 1)))];
  const sum = fs.every(f => f.algo) ? ht(fs.some(f => f.pieces) ? "fi.sumPieces" : "fi.sumChecked", { algo: ALGO[fs[0].algo] })
    : fs.every(f => f.pieces) ? ht("fi.sumPiecesOnly", { algo: ALGO[fs[0].pieceAlgo] || "" }) : "—";
  return row(te("fact.servers"), most(fs) ? ht("fi.servers", { n: most(fs) }) : "—") + row(te("fact.checksum"), sum)
    + (tor.length ? row(te("fact.torrent"), ht(most(tor) ? "fi.mlTorrentSeeds" : "fi.mlTorrent", { n: most(tor) })) : "")
    + (dirs.length ? row(te("fact.folders"), ht("fi.mlFolders", { dir: nm(dirs[0]), more: dirs.length > 1 ? "yes" : "no" })) : "");
}
function catFor(name) {
  const ext = (String(name).split(".").pop() || "").toLowerCase();
  const c = (S.settings?.categories || []).find(c => (c.exts || []).includes(ext));
  return c ? c.name : "General";
}
const errIcon = `<svg width="16" height="16" viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.5" fill="#d93a3a"/><path d="M8 4.6v4.1" stroke="#fff" stroke-width="1.6" stroke-linecap="round"/><circle cx="8" cy="11.2" r=".95" fill="#fff"/></svg>`;
function renderSidebar() {
  const c = {}, qc = {}; let unf = 0, fin = 0, err = 0;
  for (const d of S.downloads) {
    c[d.category] = (c[d.category] || 0) + 1;
    if (d.status === "completed") fin++; else unf++;
    if (d.status === "error") err++;
    if (d.queue) qc[d.queue] = (qc[d.queue] || 0) + 1;
  }
  const node = (key, label, icon, cnt, child, running) =>
    `<div class="node${child ? " child" : ""}${S.filter === key ? " active" : ""}" data-f="${key}" id="sb-${key.replace(/\W/g, "_")}" role="option" aria-selected="${S.filter === key}" title="${esc(label)}">${icon}<span>${esc(label)}</span>${running ? `<i class="run-dot" title="${te("sb.running")}"></i>` : ""}${cnt != null ? `<span class="cnt">${fmtNum(cnt)}</span>` : ""}</div>`;
  const group = (id, title, nodes) => `<div role="group" aria-labelledby="sbh-${id}"><div class="tree-h" id="sbh-${id}">${title}</div>${nodes.join("")}</div>`;
  const html = [
    group("c", t("sb.categories"), [node("all", t("sb.all"), I.folder("#e2a72e"), S.downloads.length), ...CATS.map(k => node("cat:" + k, catName(k), catIcon(k), c[k] || 0, true))]),
    group("s", t("sb.status"), [node("unfinished", t("sb.unfinished"), I.folder("#e0533b"), unf), node("finished", t("sb.finished"), I.folder("#1f9d55"), fin), node("errors", t("sb.errors"), errIcon, err)]),
    group("q", t("sb.queues"), S.queues.map(q => node("q:" + q.id, q.name, q.sync ? I.syncQ : I.listQ, qc[q.id] || 0, false, qBusy(q)))),
  ].join("");
  const sb = $("#sbList");
  if (sb._html !== html) {
    sb.innerHTML = html; sb._html = html;
    $$(".node", sb).forEach(n => n.onclick = () => { S.filter = n.dataset.f; saveUI(); renderAll(); });
    $$(".node[data-f^='q:']", sb).forEach(n => n.ondblclick = () => queueManager({ select: n.dataset.f.slice(2) }));
    const a = $(".node.active", sb);
    if (a) sb.setAttribute("aria-activedescendant", a.id); else sb.removeAttribute("aria-activedescendant");
  }
  // compact view picker for narrow windows (the sidebar is hidden there)
  const vs = $("#viewSel");
  const opts = [["all", t("sb.all")], ...CATS.map(k => ["cat:" + k, catName(k)]), ["unfinished", t("sb.unfinished")], ["finished", t("sb.finished")], ["errors", t("sb.errors")], ...S.queues.map(q => ["q:" + q.id, q.name])];
  const vh = opts.map(([v, label]) => `<option value="${v}">${esc(label)}</option>`).join("");
  if (vs._h !== vh) { vs.innerHTML = vh; vs._h = vh; }
  vs.value = S.filter;
}
// The sidebar is a listbox of views, one Tab stop: Up/Down/Home/End pick the view (the list
// follows), Enter or Space too. "New queue…" is a button after it.
(() => {
  const sb = h(`<div id="sbList" role="listbox" tabindex="0" aria-label="${te("sb.show")}"></div>`);
  const add = h(`<button type="button" class="node add-q">${I.plus}<span>${te("sb.newQueue")}</span></button>`);
  add.onclick = () => queueManager({ create: true });
  $("#sidebar").append(sb, add);
  sb.addEventListener("keydown", e => {
    const L = $$(".node[data-f]", sb), i = L.findIndex(n => n.dataset.f === S.filter), k = e.key;
    const j = { ArrowDown: i + 1, ArrowUp: i - 1, Home: 0, End: L.length - 1, PageDown: L.length - 1, PageUp: 0, Enter: i, " ": i }[k];
    if (j === undefined || e.altKey || e.ctrlKey || e.metaKey) return;
    e.preventDefault(); e.stopPropagation();
    const n = L[Math.max(0, Math.min(L.length - 1, j))];
    if (n && n.dataset.f !== S.filter) { S.filter = n.dataset.f; saveUI(); renderAll(); }
    $(".node.active", sb)?.scrollIntoView({ block: "nearest" });
  });
})();

// ------------------------------------------------------------------ table
// Every column the list can show. "on" = shown by default. w = its usual width, min = the least it
// shrinks to, pri = how long it stays as the list gets narrower (low goes first; see fitColumns).
const ALLCOLS = [
  { k: "name", w: 330, fixed: true },
  { k: "size", w: 90, min: 70, pri: 90, num: true, on: true },
  { k: "status", w: 160, min: 120, pri: 100, on: true, fixed: true },
  { k: "eta", w: 110, min: 84, pri: 70, on: true },
  { k: "speed", w: 110, min: 100, pri: 80, num: true, on: true },
  { k: "up", w: 100, min: 84, pri: 60, num: true, on: true },
  { k: "lastTry", w: 150, min: 110, pri: 20, on: true },
  { k: "desc", w: 200, min: 120, pri: 30, on: true },
  { k: "added", w: 150, min: 110, pri: 40 },
  { k: "finished", w: 150, min: 110, pri: 40 },
  { k: "category", w: 110, min: 80, pri: 50 },
  { k: "queue", w: 120, min: 80, pri: 50 },
  { k: "url", w: 260, min: 140, pri: 30 },
  { k: "dir", w: 220, min: 140, pri: 30 },
  { k: "conns", w: 100, min: 70, pri: 45, num: true },
  { k: "scan", w: 140, min: 100, pri: 35 },
];
const COLDEF = Object.fromEntries(ALLCOLS.map(c => [c.k, c]));
const colName = c => t("col." + c.k);
let COLS = ALLCOLS;
// user's column order and visibility, e.g. [["name",1],["size",1],["added",0],…]
let COLPREF = (() => { try { return JSON.parse(localStorage.getItem("zl.cols") || "null"); } catch { return null; } })();
function colPrefs() {
  const known = new Set((COLPREF || []).map(x => x[0]));
  const base = (COLPREF || []).filter(x => COLDEF[x[0]]);
  for (const c of ALLCOLS) if (!known.has(c.k)) base.push([c.k, c.on || c.fixed ? 1 : 0]);
  return base;
}
function saveColPrefs(p) { COLPREF = p; try { localStorage.setItem("zl.cols", JSON.stringify(p)); } catch {} }
// fitColumns decides which columns show, and how wide, in the room the list has. The file name comes
// first: it gets up to NAME_WANT, and the other columns give way for it. They shrink a little, then the
// least important drop out (right-most first when equal). Only when just the file name and status are
// left do those squeeze further, down to the least the name can have (NAME_FLOOR).
const NAME_WANT = 300, NAME_FLOOR = 110;
function fitColumns() {
  const wrap = $("#tablewrap"), avail = wrap ? wrap.clientWidth : innerWidth;
  const want = Math.max(150, Math.min(NAME_WANT, Math.round(avail * .45))); // (a narrow list can't spare 300)
  const pref = c => COLW[c.k] || c.w, soft = c => Math.min(pref(c), Math.max(c.min, Math.round(pref(c) * .85)));
  const cols = colPrefs().filter(([k, on]) => on || COLDEF[k].fixed).map(([k]) => COLDEF[k]);
  const others = () => cols.filter(c => c.k !== "name");
  const sum = f => others().reduce((a, c) => a + f(c), 0);
  while (want + sum(soft) > avail) {
    const drop = others().filter(c => !c.fixed).reduce((lo, c) => !lo || c.pri <= lo.pri ? c : lo, null);
    if (!drop) break;
    cols.splice(cols.indexOf(drop), 1);
  }
  const tight = want + sum(soft) > avail; // (only the fixed ones are left, and still too wide)
  const nameMin = tight ? NAME_FLOOR : want, lowest = tight ? c => Math.min(pref(c), c.min) : soft;
  const room = avail - nameMin, full = sum(pref), least = sum(lowest), widths = {};
  const t = full <= room ? 1 : Math.max(0, Math.min(1, (room - least) / (full - least)));
  for (const c of others()) widths[c.k] = Math.floor(lowest(c) + (pref(c) - lowest(c)) * t);
  return { cols, widths, nameMin };
}
let COLFIT = null;
const activeCols = () => (COLFIT = fitColumns()).cols;
function refreshColumns() { colKey = activeCols().map(c => c.k).join(); buildHead(); $("#tbody").innerHTML = ""; renderTable(); }
function setColumnOn(k, on) { saveColPrefs(colPrefs().map(x => x[0] === k ? [k, on ? 1 : 0] : x)); refreshColumns(); }
function columnMenuItems() {
  return [
    ...colPrefs().filter(([k]) => !COLDEF[k].fixed).map(([k, on]) => {
      const c = COLDEF[k], hiddenBySize = !COLS.some(x => x.k === k); // (ticked, but the list is too narrow for it now)
      return menuItem(colName(c), hiddenBySize && on ? t("col.needsRoom") : "", () => setColumnOn(k, !on), { checked: !!on });
    }),
    SEP,
    menuItem(t("col.arrange"), "", () => columnsDialog()),
    menuItem(t("col.reset"), "", () => { saveColPrefs(null); COLW = {}; saveUI(); refreshColumns(); }),
  ];
}
var COLW = COLW || {};
// colWidths sets the column widths from the fit; the file-name column has none, so it takes whatever is left
// (and the table never gets narrower than it needs, so the name can't be squeezed away: the list scrolls sideways instead)
function colWidths() {
  $("#cols").innerHTML = COLS.map(c => c.k === "name" ? `<col>` : `<col style="width:${COLFIT.widths[c.k]}px">`).join("");
  $("#tbl").style.minWidth = COLFIT.nameMin + COLS.reduce((a, c) => a + (COLFIT.widths[c.k] || 0), 0) + "px";
}
function buildHead() {
  COLS = activeCols();
  colWidths();
  const tr = $("#thead");
  const had = document.activeElement && document.activeElement.closest("#thead th"); // (keyboard focus stays on its column)
  if (!COLS.some(c => c.k === LIST.head)) LIST.head = COLS[0].k;
  // one Tab stop for the headers (Left/Right move between them); aria-sort says how the list is sorted
  tr.innerHTML = COLS.map(c => `<th data-k="${c.k}"${c.num ? ' class="num"' : ""} tabindex="${c.k === LIST.head ? 0 : -1}"${S.sort.col === c.k ? ` aria-sort="${S.sort.dir > 0 ? "ascending" : "descending"}"` : ""} title="${te("col.sortTip")}">${esc(colName(c))}<span class="arrow" aria-hidden="true">${S.sort.col === c.k ? (S.sort.dir > 0 ? "▲" : "▼") : ""}</span><span class="rs"></span></th>`).join("");
  if (had) $(`th[data-k="${LIST.head}"]`, tr)?.focus();
  tr.oncontextmenu = e => { e.preventDefault(); showMenuAt({ getBoundingClientRect: () => ({ left: e.clientX, bottom: e.clientY }) }, columnMenuItems()); };
  $$("th", tr).forEach((th, i) => {
    th.onclick = e => {
      if (e.target.classList.contains("rs") || th._dragged) { th._dragged = false; return; }
      sortBy(th.dataset.k);
    };
    th.onmousedown = e => { if (e.button === 0 && !e.target.classList.contains("rs")) startColumnDrag(e, th); };
    if (COLS[i].k === "name") { $(".rs", th).remove(); return; }
    $(".rs", th).onmousedown = e => {
      e.preventDefault(); e.stopPropagation();
      const col = $$("#cols col")[i]; const x0 = e.clientX; const w0 = col.getBoundingClientRect().width || th.offsetWidth;
      const mv = ev => { const w = Math.max(50, w0 + ev.clientX - x0); col.style.width = w + "px"; COLW[COLS[i].k] = w; };
      const up = () => { removeEventListener("mousemove", mv); removeEventListener("mouseup", up); saveUI(); };
      addEventListener("mousemove", mv); addEventListener("mouseup", up);
    };
  });
}
function sortBy(k) { S.sort = { col: k, dir: S.sort.col === k ? -S.sort.dir : 1 }; LIST.head = k; saveUI(); buildHead(); renderTable(); }
// Drag a header sideways to move the column.
function startColumnDrag(e, th) {
  const x0 = e.clientX, from = th.dataset.k;
  let moving = false, target = null, after = false;
  const mark = $("#colmark");
  const mv = ev => {
    if (!moving && Math.abs(ev.clientX - x0) < 6) return;
    moving = true; th.classList.add("dragging");
    const ths = $$("#thead th");
    const over = ths.find(x => { const r = x.getBoundingClientRect(); return ev.clientX >= r.left && ev.clientX < r.right; }) || (ev.clientX < ths[0].getBoundingClientRect().left ? ths[0] : ths[ths.length - 1]);
    const r = over.getBoundingClientRect(), wrap = $("#tablewrap").getBoundingClientRect();
    target = over.dataset.k; after = ev.clientX > r.left + r.width / 2;
    mark.style.display = "block";
    mark.style.left = (after ? r.right : r.left) - wrap.left + $("#tablewrap").scrollLeft - 1 + "px";
    mark.style.height = r.height + "px";
  };
  const up = () => {
    removeEventListener("mousemove", mv); removeEventListener("mouseup", up);
    mark.style.display = "none"; th.classList.remove("dragging");
    if (!moving || !target || target === from) return;
    th._dragged = true;
    const p = colPrefs(), item = p.find(x => x[0] === from);
    const rest = p.filter(x => x[0] !== from);
    let idx = rest.findIndex(x => x[0] === target) + (after ? 1 : 0);
    rest.splice(idx, 0, item);
    saveColPrefs(rest); refreshColumns();
  };
  addEventListener("mousemove", mv); addEventListener("mouseup", up);
}
function columnsDialog() {
  let pref = colPrefs(), cur = null;
  const el = dialog({
    title: t("cols.title"), width: 420, key: "cols",
    body: `<div class="hint" style="margin-top:0">${te("cols.hint")}</div>
      <div class="qlist cols-list" id="c-list" role="listbox" aria-label="${te("cols.title")}"></div>
      <div class="inline" style="margin-top:8px"><button class="btn sm" id="c-up">${te("cols.up")}</button><button class="btn sm" id="c-down">${te("cols.down")}</button><span class="grow"></span><button class="btn sm" id="c-reset">${te("btn.reset")}</button></div>`,
    buttons: [
      { label: t("btn.ok"), primary: true, fn: el => { saveColPrefs(pref); refreshColumns(); el.close(); } },
      { label: t("btn.cancel"), fn: el => el.close() },
    ],
  });
  const draw = () => {
    $("#c-list", el).innerHTML = pref.map(([k, on]) => {
      const c = COLDEF[k];
      return `<label class="qitem${cur === k ? " sel" : ""}" data-k="${k}"><input type="checkbox"${on || c.fixed ? " checked" : ""}${c.fixed ? " disabled" : ""}><span>${esc(colName(c))}</span><span class="s">${esc(t(c.fixed ? "cols.always" : "cols.room"))}</span></label>`;
    }).join("");
    $$(".qitem", el).forEach(r => {
      r.onclick = e => { if (e.target.tagName !== "INPUT") { cur = r.dataset.k; draw(); } };
      $("input", r).onchange = ev => { pref = pref.map(x => x[0] === r.dataset.k ? [x[0], ev.target.checked ? 1 : 0] : x); cur = r.dataset.k; draw(); };
    });
  };
  const move = d => { const i = pref.findIndex(x => x[0] === cur); const j = i + d; if (i < 0 || j < 0 || j >= pref.length) return; [pref[i], pref[j]] = [pref[j], pref[i]]; draw(); };
  $("#c-up", el).onclick = () => move(-1);
  $("#c-down", el).onclick = () => move(1);
  $("#c-reset", el).onclick = () => { pref = ALLCOLS.map(c => [c.k, c.on || c.fixed ? 1 : 0]); cur = null; draw(); };
  draw();
}
// ---- the list in view
// LIST.ids: the downloads that pass the filter and search, sorted, as IDs. It's worked out again
// only when the list or the view changed (LIST.dirty, or a different filter/search/sort), and only
// the rows on screen exist in the page (renderRows), so thousands of downloads cost no more than
// a screenful. LIST.focus is the keyboard's row (the grid's active descendant).
const LIST = { ids: [], key: null, dirty: true, ix: null, pos: null, rows: new Map(), spare: [], top: null, bot: null, rowH: 28, g: 0, gKey: "", focus: null, head: "name", raf: 0 };
function inView(d) {
  const f = S.filter;
  if (f.startsWith("cat:") ? d.category !== f.slice(4) : f === "unfinished" ? d.status === "completed" : f === "finished" ? d.status !== "completed" : f === "errors" ? d.status !== "error" : f.startsWith("q:") && d.queue !== f.slice(2)) return false;
  return !S.search || (d.name + " " + d.url + " " + (d.desc || "")).toLowerCase().includes(S.search);
}
// sortKey is the sort column's key, or null in a queue (queue order).
function sortKey() {
  if (S.filter.startsWith("q:")) return null;
  return {
    name: d => d.name.toLowerCase(), size: d => d.size, status: d => d.status === "completed" ? 101 : pct(d),
    eta: d => etaOf(d), speed: d => d.speed, up: d => d.torrent ? d.torrent.up : 0, lastTry: d => d.lastTry || d.added, desc: d => descText(d).toLowerCase(), added: d => d.added,
    finished: d => d.finished || "", category: d => d.category, queue: d => qName(d.queue).toLowerCase(), url: d => d.url,
    dir: d => (d.dir || "").toLowerCase(), conns: d => d.workers || 0, scan: d => d.scanState || "",
  }[S.sort.col] || (d => d.added);
}
// rowMoves: a download's new view may take it into, out of or across the list in view.
function rowMoves(a, b) {
  if (inView(a) !== inView(b)) return true;
  const k = sortKey();
  return !!k && k(a) !== k(b);
}
function listIds() {
  const key = [S.filter, S.search, S.sort.col, S.sort.dir].join("\n");
  if (LIST.src !== S.downloads) { LIST.src = S.downloads; LIST.dirty = true; } // (a whole new list, not deltas)
  if (!LIST.dirty && key === LIST.key) return LIST.ids;
  const L = S.downloads.filter(inView), k = sortKey(), dir = S.sort.dir;
  let ids;
  if (!k) ids = L.map(d => d.id);
  else { // (keys worked out once each; ties keep the list's order)
    const ks = L.map(k), ix = L.map((_, i) => i);
    ix.sort((a, b) => { const x = ks[a], y = ks[b]; return (x < y ? -1 : x > y ? 1 : 0) * dir || a - b; });
    ids = ix.map(i => L[i].id);
  }
  const was = LIST.focus ? idx(LIST.focus) : -1, sameView = key === LIST.key;
  if (!sameView) LIST.g++; // (rows show the queue badge only outside queues)
  LIST.ids = ids; LIST.key = key; LIST.dirty = false; LIST.ix = null;
  // the keyboard's row went away: the one now in its place takes over
  if (LIST.focus && idx(LIST.focus) < 0) LIST.focus = sameView && was >= 0 && ids.length ? ids[Math.min(was, ids.length - 1)] : null;
  return ids;
}
// idx is a download's place in the list in view (-1: not in it).
function idx(id) {
  if (!LIST.ix) LIST.ix = new Map(LIST.ids.map((x, i) => [x, i]));
  const i = LIST.ix.get(id);
  return i === undefined ? -1 : i;
}
function visible() { return listIds().map(id => S.byId.get(id)); }
// speedCell is the Transfer rate: what comes in. A torrent that only shares has what it sends there
// too, unless the Upload speed column shows it.
function speedCell(d) {
  const up = upText(d);
  if (d.torrent && d.torrent.seeding) return up && !COLS.some(c => c.k === "up") ? `<span title="${te("row.uploadTip")}">↑ ${up}</span>` : "";
  return fmtSpeed(d.speed) + (d.limitKB > 0 && d.status === "downloading" ? ` <span class="unit" title="${esc(t("row.limitedTip", { rate: fmtKBps(d.limitKB) }))}">⌁</span>` : "");
}
// upText is what a torrent sends to other peers, while it downloads as well as while it shares.
const upText = d => d.torrent && d.torrent.up > 0 ? fmtSpeed(d.torrent.up) : "";
// statusCell is a row's status; a finished torrent that wouldn't share says why when you point at it.
function statusCell(d) {
  const why = d.status === "completed" && d.torrent && d.torrent.noShare;
  return why && NO_SHARE_WORD[why.why] ? `<span title="${esc(t("sh.tip", { why: t("sh.no." + why.why, { file: why.file || "" }) }))}">${esc(statusText(d))}</span>` : esc(statusText(d));
}
function rowCells(d) {
  const p = pct(d);
  const stCls = d.status === "completed" ? (d.scanState === "threat" || d.check && d.check.state === "bad" ? "err" : "done") : d.status === "error" ? (d.link === "refreshing" ? "paused" : "err") : (d.status === "paused" || d.status === "queued" || (d.status === "downloading" && (capHeld() || quotaHeld()))) ? "paused" : "";
  const showBar = d.status !== "completed" && (d.size > 0 || (isHLS(d) && d.parts > 0)) && (d.done > 0 || d.status === "downloading");
  const qn = d.queue ? qName(d.queue) : "";
  return {
    name: `<div class="fn">${catIcon(d.category)}<span><bdi>${esc(dname(d))}</bdi></span>${qn && !S.filter.startsWith("q:") ? `<em class="qbadge" title="${te("row.inQueue", { name: qn })}">${esc(qn)}</em>` : ""}${d.private ? `<em class="qbadge pv" title="${te("row.privateTip")}">${te("row.private")}</em>` : ""}</div>`,
    size: sizeText(d),
    status: `${statusCell(d)}${showBar ? progressBar(p, progressText(d), { label: dname(d) }) : ""}`,
    eta: d.status === "downloading" && !d.stage ? fmtTime(etaOf(d)) : "",
    speed: speedCell(d),
    up: upText(d) && `<span title="${te("row.uploadTip")}">${upText(d)}</span>`,
    lastTry: fmtDate(d.lastTry || d.added),
    desc: esc(descText(d)),
    added: fmtDate(d.added),
    finished: d.status === "completed" ? fmtDate(d.finished) : "",
    category: esc(catName(d.category)),
    queue: esc(qn),
    url: `<span title="${esc(d.url)}">${esc(d.url)}</span>`,
    dir: `<span title="${esc(d.dir || "")}">${esc(d.dir || "")}</span>`,
    conns: d.status === "downloading" ? String(d.workers || 0) : "",
    scan: scanCell(d),
    cls: stCls,
  };
}
const SCAN_STATES = ["waiting", "scanning", "clean", "threat", "failed"];
const scanCell = d => d.scanState ? `<span class="scan ${d.scanState}" title="${esc(d.scanNote || "")}">${SCAN_STATES.includes(d.scanState) ? esc(t("scan." + d.scanState)) : ""}</span>` : "";
const descText = d => d.desc || (d.status === "error" ? d.error : "") || (d.scanState === "threat" ? d.scanNote : "") || d.note || d.syncNote || "";
function renderTable() {
  const n = listIds().length;
  renderRows();
  const em = $("#empty");
  if (!S.downloads.length) {
    em.style.display = "block";
    em.innerHTML = I.add.replace('width="24" height="24"', 'width="54" height="54"') + ht("empty.none", { add: raw(`<u style="cursor:pointer" onclick="addUrlDialog()">${te("a.addLink")}</u>`) });
  } else if (!n) { em.style.display = "block"; em.innerHTML = ht("empty.view"); }
  else em.style.display = "none";
}
// ---- virtual scrolling: the rows on screen (plus a few either side) between two spacer rows.
// Rows have a fixed height; a row leaving the screen is reused for one coming in, and a row is
// drawn again only when its download, or what rows show from elsewhere (LIST.g), changed.
const BUF = 8;
function resetRows() { // (new columns: new rows)
  const tb = $("#tbody"), pad = `<tr class="vpad" aria-hidden="true"><td colspan="${COLS.length}"></td></tr>`;
  tb.innerHTML = pad + pad;
  [LIST.top, LIST.bot] = tb.children;
  LIST.rows = new Map(); LIST.spare = [];
}
function newRow() {
  const tr = document.createElement("tr");
  tr.innerHTML = COLS.map(c => `<td${c.num ? ' class="num"' : ""}></td>`).join("");
  tr._c = [];
  return tr;
}
function fillRow(tr, d, i) {
  if (tr.dataset.id !== d.id) { tr.dataset.id = d.id; tr.id = "r-" + d.id; }
  if (tr._i !== i) { tr._i = i; tr.setAttribute("aria-rowindex", i + 2); } // (the header is row 1)
  if (tr._d !== d || tr._g !== LIST.g || d.tryAgain || d.retryAt) { // (those count down)
    tr._d = d; tr._g = LIST.g;
    const cells = rowCells(d), tds = tr.children;
    COLS.forEach((c, j) => {
      if (tr._c[j] !== cells[c.k]) { tds[j].innerHTML = cells[c.k]; tr._c[j] = cells[c.k]; }
      if (c.k === "status") tds[j].className = "st " + cells.cls;
      if (c.k === "desc") tds[j].title = descText(d);
      if (c.k === "name") tds[j].title = d.name + "\n" + d.url;
    });
  }
  const s = S.selected.has(d.id), f = d.id === LIST.focus;
  if (tr._s !== s) { tr._s = s; tr.classList.toggle("sel", s); tr.setAttribute("aria-selected", s); }
  if (tr._f !== f) { tr._f = f; tr.classList.toggle("cur", f); }
}
function renderRows() {
  const tb = $("#tbody"), ids = listIds(), n = ids.length;
  if (!LIST.top || LIST.top.parentNode !== tb) resetRows(); // (the columns changed: refreshColumns empties it)
  if (!LIST.view) listSize();
  const H = LIST.rowH, view = Math.max(H, LIST.view), top = Math.min(LIST.scrollTop, Math.max(0, n * H - view)); // (as the browser will clamp it)
  const first = Math.max(0, Math.min(n, Math.floor(top / H)) - BUF), last = Math.min(n, Math.ceil((top + view) / H) + BUF);
  const want = new Set(ids.slice(first, last)), rows = new Map();
  for (const [id, tr] of LIST.rows) if (!want.has(id)) { tr.remove(); LIST.spare.push(tr); }
  let prev = LIST.top;
  for (let i = first; i < last; i++) {
    const id = ids[i], d = S.byId.get(id);
    if (!d) { LIST.dirty = true; continue; } // (gone since the list was worked out: next frame)
    const tr = LIST.rows.get(id) || LIST.spare.pop() || newRow();
    fillRow(tr, d, i);
    rows.set(id, tr);
    if (prev.nextSibling !== tr) tb.insertBefore(tr, prev.nextSibling);
    prev = tr;
  }
  LIST.rows = rows; LIST.spare.length = Math.min(LIST.spare.length, 60);
  const pad = (tr, px) => { tr.hidden = !px; tr.firstChild.style.height = px + "px"; };
  pad(LIST.top, first * H); pad(LIST.bot, (n - last) * H);
  const t = $("#tbl");
  if (t._n !== n) { t._n = n; t.setAttribute("aria-rowcount", n + 1); }
  if (LIST.focus && rows.has(LIST.focus)) t.setAttribute("aria-activedescendant", "r-" + LIST.focus); else t.removeAttribute("aria-activedescendant");
  // the real row height (fonts and zoom can change it), measured once there's a row
  if (!LIST.measured && rows.size) {
    LIST.measured = true;
    const r = prev.getBoundingClientRect().height;
    if (r && Math.abs(r - H) > .5) { LIST.rowH = r; renderRows(); }
  }
}
// listSize notes the list's size and scroll position, so drawing rows needn't ask for a layout.
function listSize() {
  const w = $("#tablewrap");
  LIST.view = w.clientHeight - $("#thead").offsetHeight; LIST.scrollTop = w.scrollTop;
}
$("#tablewrap").addEventListener("scroll", () => {
  LIST.scrollTop = $("#tablewrap").scrollTop;
  if (!LIST.raf) LIST.raf = requestAnimationFrame(() => { LIST.raf = 0; renderRows(); });
}, { passive: true });
new ResizeObserver(() => { listSize(); LIST.measured = false; renderRows(); }).observe($("#tablewrap"));
// rows come and go as the list scrolls, so their mouse events are handled here
$("#tbody").addEventListener("mousedown", e => { const tr = e.target.closest("tr[data-id]"); if (tr) rowDown.call(tr, e); });
$("#tbody").addEventListener("dblclick", e => { const tr = e.target.closest("tr[data-id]"); if (tr) rowOpen(tr.dataset.id); });
$("#tbody").addEventListener("contextmenu", e => { if (e.target.closest("tr[data-id]")) rowCtx(e); });
function rowDown(e) {
  const id = this.dataset.id;
  LIST.focus = id;
  if (e.button === 2 && S.selected.has(id)) return renderRows();
  if (e.shiftKey && S.anchor) {
    const ids = listIds(), b = idx(id); let a = idx(S.anchor); if (a < 0) a = b;
    if (!e.ctrlKey && !e.metaKey) S.selected.clear();
    ids.slice(Math.min(a, b), Math.max(a, b) + 1).forEach(x => S.selected.add(x));
  } else if (e.ctrlKey || e.metaKey) {
    S.selected.has(id) ? S.selected.delete(id) : S.selected.add(id); S.anchor = id;
  } else { S.selected = new Set([id]); S.anchor = id; }
  renderTable(); updateToolbar();
}
let colKey = activeCols().map(c => c.k).join();
// the list lays its columns out again whenever its own width changes (window, zoom, side panel shown or hidden)
new ResizeObserver(() => {
  const k = activeCols().map(c => c.k).join();
  if (k !== colKey) { colKey = k; buildHead(); $("#tbody").innerHTML = ""; renderTable(); } else colWidths();
}).observe($("#tablewrap"));
addEventListener("resize", () => $$(".dlg").forEach(keepOnScreen));
$("#tablewrap").addEventListener("mousedown", e => { // empty space clears the selection (its scroll bars don't)
  const w = e.currentTarget;
  if (e.target === w && (e.offsetX >= w.clientWidth || e.offsetY >= w.clientHeight)) return;
  if (!e.target.closest("tr[data-id]") && !e.target.closest("thead")) { S.selected.clear(); renderTable(); updateToolbar(); }
});
function rowOpen(id) {
  const d = S.byId.get(id); if (!d) return;
  if (d.status === "completed") api("action", { action: "open", ids: [id] });
  else progressDialog(id);
}
function rowCtx(e) {
  e.preventDefault();
  const sl = sel(); if (!sl.length) return;
  const one = sl.length === 1 ? sl[0] : null;
  const anyDone = sl.some(d => d.status === "completed");
  const anyActive = sl.some(d => ["downloading", "connecting", "queued"].includes(d.status));
  const items = [
    menuItem(t("a.open"), "Enter", () => act("open"), { disabled: !one || one.status !== "completed" }),
    menuItem(t("a.openFolder"), "", () => act("folder")),
    menuItem(t("ctx.moveTo"), "", () => moveDialog(sl.map(d => d.id)), { disabled: sl.every(d => d.move) }),
    menuItem(t("ctx.showProgress"), "", () => sl.forEach(d => progressDialog(d.id)), { disabled: anyDone && sl.length === 1 && !(one.torrent && one.torrent.seeding) }),
    SEP,
    menuItem(t("a.resume"), "", () => act("start"), { disabled: sl.every(d => d.status === "completed" && !canShareAgain(d)) }),
    menuItem(t("a.pause"), "", () => act("pause"), { disabled: !anyActive }),
    menuItem(t("a.redownload"), "", () => redownload(sl)),
    ...(sl.some(d => d.torrent) ? [
      menuItem(t("a.shareAgain"), "", () => act("start", sl.filter(canShareAgain).map(d => d.id)), { disabled: !sl.some(canShareAgain) }),
      menuItem(t("ctx.stopSharing"), "", () => act("stopSharing", sl.filter(d => d.torrent && d.torrent.seeding).map(d => d.id)), { disabled: !sl.some(d => d.torrent && d.torrent.seeding) }),
      menuItem(t("ctx.chooseFiles"), "", () => propsDialog(one, "files"), { disabled: !one || !one.torrent || one.torrent.needMeta || (one.torrent.fileCount || 0) < 2 }),
      menuItem(t("ctx.checkFiles"), "", () => act("recheck", sl.filter(d => d.torrent && !d.torrent.needMeta).map(d => d.id)), { disabled: !sl.some(d => d.torrent && !d.torrent.needMeta && !d.torrent.checking) }),
      ...orderItems(sl.filter(d => d.torrent)),
    ] : []),
    ...(sl.every(d => d.torrent) ? [] : [
      menuItem(t("ctx.refreshLink"), "", () => refreshLink(one), { disabled: !one || one.status === "completed" || isHLS(one) || !!one.torrent }),
      menuItem(t("ctx.openInBrowser"), "", () => act("browse"), { disabled: !one || !!one.torrent }),
      menuItem(t("ctx.checkFile"), "", () => propsDialog(one, "sum"), { disabled: !one || one.status !== "completed" || isHLS(one) || !!one.torrent }),
    ]),
    SEP,
    ...S.queues.filter(q => q.sync || !sl.every(d => d.status === "completed")).map(q =>
      menuItem(th("a.addTo", { name: nm(q.name) }), q.sync ? t("queue.syncTag") : "", () => api("action", { action: "queueAdd", ids: sl.map(d => d.id), queue: q.id }), { disabled: sl.every(d => d.queue === q.id) })),
    ...(sl.some(d => d.inQueue) ? [menuItem(t("ctx.removeFromQueue"), "", () => act("queueRemove"))] : []),
    menuItem(t("ctx.moveUp"), "", () => act("up")),
    menuItem(t("ctx.moveDown"), "", () => act("down")),
    SEP,
    menuItem(t(sl.every(d => d.torrent) ? "a.copyMagnet" : "a.copyLink"), "", () => { navigator.clipboard?.writeText(sl.map(d => d.torrent?.magnet || d.url).join("\n")); toast(t(sl.every(d => d.torrent) ? "ctx.magnetCopied" : "ctx.linkCopied")); }),
    menuItem(t("ctx.scanNow"), "", () => act("scan", sl.filter(d => d.status === "completed").map(d => d.id)), { disabled: !sl.some(d => d.status === "completed") }),
    menuItem(t("ctx.exportSelected"), "", () => exportDialog(sl.map(d => d.id))),
    menuItem(t("a.removeFromList"), "Del", () => removeSel(false)),
    menuItem(t("a.deleteFile"), "Shift+Del", () => removeSel(true)),
    SEP,
    menuItem(t("a.properties"), "Alt+Enter", () => propsDialog(one), { disabled: !one }),
  ];
  const m = $("#ctx");
  fillMenu(m, items);
  m.style.display = "block";
  placeMenu(m, e.clientX, e.clientY);
  menuOpened(m);
}
// orderItems are the torrent menu's picking options (ticked when every torrent chosen has it).
function orderItems(tors) {
  const all = k => tors.every(d => d.torrent[k]);
  const set = (k, on) => tors.forEach(d => api("torrent/files", { id: d.id, [k]: on }));
  return [
    menuItem(t("tor.seq"), "", () => set("seq", !all("seq")), { checked: all("seq") }),
    menuItem(t("tor.ends"), "", () => set("ends", !all("ends")), { checked: all("ends") }),
  ];
}
const sel = () => [...S.selected].map(id => S.byId.get(id)).filter(Boolean);
// canShareAgain: a finished torrent that isn't sharing (Resume does the same for it).
const canShareAgain = d => !!d.torrent && d.status === "completed" && !d.torrent.seeding && !d.move;
function act(action, ids, sure) {
  ids = ids || [...S.selected];
  if (!ids.length) return;
  const p = S.plan;
  const ask = () => confirmDialog(t("cf.freeHoursTitle"), t("cf.freeHoursBody", { window: planWindowText(p) }), t("cf.freeHoursNow"), () => act(action, ids, true));
  if (action === "start" && !sure && p && p.on && !p.in && ids.some(id => S.byId.get(id)?.queue === PLAN_Q)) return ask();
  // (the app asks too, whatever the window knew: see gate.go)
  return api("action", { action, ids, sure: !!sure }).then(r => { if (r && r.ask === "freeHours") ask(); return r; });
}
// startQueue starts a queue. The Free data hours queue asks first outside its free hours: the app
// won't start it there without a yes.
async function startQueue(id, sure) {
  const r = await api("queue", { action: "start", id, sure: !!sure });
  if (r && r.ask === "freeHours")
    confirmDialog(t("cf.queueFreeHoursTitle"), t("cf.queueFreeHoursBody", { window: planWindowText(S.plan) }), t("cf.startNow"), () => startQueue(id, true));
  return r;
}
function pauseAll() { act("pause", S.downloads.filter(d => ["downloading", "connecting", "queued"].includes(d.status)).map(d => d.id)); S.queues.filter(qBusy).forEach(q => api("queue", { action: "stop", id: q.id })); }
function resumeAll() { act("start", S.downloads.filter(d => d.status === "paused" || d.status === "error").map(d => d.id)); }
function removeSel(withFile) {
  const sl = sel(); if (!sl.length) return;
  const ids = sl.map(d => d.id); // what you chose now, whatever is selected when you confirm
  const unfinished = sl.some(d => d.status !== "completed");
  const vars = { n: sl.length, name: nm(dname(sl[0])), partial: !unfinished ? "none" : sl.length === 1 ? "single" : "many" };
  confirmDialog(th(withFile ? "cf.deleteTitle" : "cf.removeTitle", vars), th(withFile ? "cf.deleteBody" : "cf.removeBody", vars), t(withFile ? "btn.delete" : "btn.remove"), () => { act(withFile ? "removeFile" : "remove", ids); ids.forEach(id => S.selected.delete(id)); renderTable(); updateToolbar(); }, { danger: withFile || sl.some(d => d.status !== "completed" && d.done > 0) });
}
// redownload starts downloads again from the beginning, asking first when that discards progress.
// A torrent checks its files instead: the pieces already there stay, so it isn't asked about.
function redownload(list) {
  const ids = list.map(d => d.id), losing = list.filter(d => d.status !== "completed" && d.done > 0 && !d.torrent);
  if (!ids.length) return;
  if (!losing.length) return act("redownload", ids);
  const vars = { n: losing.length, name: nm(dname(losing[0])), tor: list.filter(d => d.torrent).length };
  confirmDialog(th("cf.redownloadTitle"), th("cf.redownloadBody", vars), t("a.redownload"), () => act("redownload", ids), { danger: true });
}
function removeCompleted() {
  const ids = S.downloads.filter(d => d.status === "completed").map(d => d.id);
  if (ids.length) confirmDialog(th("cf.removeCompletedTitle"), th("cf.removeCompletedBody", { n: ids.length }), t("btn.remove"), () => act("remove", ids));
}
async function toggleLimiter() {
  const st = await api("settings"); st.speedLimitOn = !st.speedLimitOn; S.settings = await api("settings", st);
  toast(t(st.speedLimitOn ? "n.limiterOn" : "n.limiterOff", { rate: fmtKBps(st.speedLimitKB) }));
}

// ------------------------------------------------------------------ status bar
function renderStatus() {
  const active = S.downloads.filter(d => d.status === "downloading" || d.status === "connecting").length;
  const conns = S.downloads.reduce((a, d) => a + (d.workers || 0), 0);
  const unf = S.downloads.filter(d => d.status !== "completed").length;
  patchHTML($("#sb-main"),
    `<span>${ht("bar.downloading", { n: active })}</span><span class="opt">${ht("bar.connections", { n: conns })}</span><span class="opt">${ht("bar.unfinished", { n: unf })}</span>` +
    `<span class="grow"></span>${powerStatus()}<span title="${esc(S.queues.filter(qBusy).map(q => q.name).join(", "))}"><span class="dot ${S.queueRunning ? "on" : ""}"></span>${queueStatusText()}</span>` +
    capStatus() + quotaStatus() + planStatus() + upStatus() + profileStatus() +
    `<span class="opt">${S.limitOn ? ht("bar.limiterOn", { rate: fmtKBps(S.limitKB) }) : ht("bar.limiterOff")}</span><span>${ht("bar.total", { speed: fmtSpeed(S.totalSpeed, true) })}</span>`);
  // a hold-up worth saying out loud, once each time it starts
  const cap = capHeld();
  if (S.capWas === false && cap) { const m = t("n.capReached", { wait: fmtTime(S.cap.freeIn) || t("time.aMoment") }); note(m, "warn"); say(m, true); }
  S.capWas = cap;
  document.title = S.totalSpeed > 0 ? t("app.titleSpeed", { speed: fmtSpeed(S.totalSpeed) }) : t("app.title");
}
// patchHTML redraws a container in place, replacing only the parts that changed, so the numbers can tick
// while a button in it (the profile menu, the quota) keeps the keyboard focus.
function patchHTML(el, html) {
  const tpl = document.createElement("template"); tpl.innerHTML = html;
  const next = [...tpl.content.childNodes], cur = [...el.childNodes];
  next.forEach((n, i) => {
    const c = cur[i];
    if (!c) el.appendChild(n);
    else if (c.isEqualNode(n)) return;
    else if (c.nodeType === 1 && n.nodeType === 1 && c.tagName === n.tagName && c.dataset.sb && c.dataset.sb === n.dataset.sb) { // (a button: same one, new words)
      [...c.attributes].forEach(a => c.removeAttribute(a.name)); [...n.attributes].forEach(a => c.setAttribute(a.name, a.value));
      c.innerHTML = n.innerHTML;
    } else el.replaceChild(n, c);
  });
  cur.slice(next.length).forEach(c => c.remove());
}
// upStatus shows uploading to other peers while torrents run or share.
function upStatus() {
  const ts = S.downloads.filter(d => d.torrent && (d.torrent.seeding || d.status === "downloading"));
  if (!ts.length) return "";
  const up = ts.reduce((a, d) => a + (d.torrent.up || 0), 0), sharing = ts.filter(d => d.torrent.seeding).length, held = ts.some(d => d.torrent.held);
  if (!up && !sharing && !held) return "";
  if (!up && held) return `<span class="opt" title="${te("bar.uploadPausedTip")}">${ht("bar.uploadPaused")}</span>`;
  return `<span class="opt" title="${esc(sharing ? t("bar.sharingTip", { n: sharing }) : t("row.uploadTip"))}">${ht("bar.upload", { speed: fmtSpeed(up, true) })}</span>`;
}
function planStatus() {
  let out = "";
  const p = S.plan;
  if (p && p.metered) out += `<span class="cap-hit" title="${te("bar.meteredTip")}">${p.paused ? ht("bar.meteredPaused", { n: p.paused }) : ht("bar.metered")}</span>`;
  if (p && p.on) out += `<span class="opt2${p.in ? " plan-in" : ""}" title="${esc(planEstimateText(p))}">${p.in ? ht("bar.freeUntil", { time: hhmm(p.to) }) : ht("bar.freeIn", { wait: fmtIn((new Date(p.from) - Date.now()) / 1000) })}</span>`;
  const pl = S.polite;
  if (pl && pl.on && (pl.rate > 0 || pl.upRate > 0)) { // (with several connections, each one's own figures in the tooltip)
    const full = t("bar.fullSpeed");
    const lines = (pl.lines || []).filter(l => l.rate > 0 || l.upRate > 0).map(l => t("bar.politeLine", { name: l.name, down: fmtSpeed(l.rate) || full, up: fmtSpeed(l.upRate) || full, ms: l.queueMs }));
    const tip = t(pl.upRate > 0 ? "bar.politeTipUp" : "bar.politeTip", { ms: pl.queueMs, up: fmtSpeed(pl.upRate) }) + (lines.length ? "\n" + lines.join("\n") : "");
    out += `<span class="opt" title="${esc(tip)}">${ht("bar.polite", { rate: pl.rate > 0 ? fmtSpeed(pl.rate) : t("bar.politeUp", { rate: fmtSpeed(pl.upRate) }) })}</span>`;
  }
  if (S.net && S.net.length > 1) {
    const used = S.net.filter(n => n.use && !n.bad);
    out += `<span class="opt" title="${esc(used.map(n => t("bar.netLine", { name: n.name, speed: fmtSpeed(n.speed, true) })).join(" · "))}">${ht("bar.networks", { n: used.length })}</span>`;
  }
  return out;
}
// ---- traffic profiles and data quotas (see gate.go)
// capHeld: the data cap holds downloads back now (unless it counts only uploads).
const capHeld = () => !!(S.cap && S.cap.blocked && (!S.quota || S.quota.count !== "up"));
// quotaHeld: a used-up quota holds downloads back now.
function quotaHeld() {
  const qv = S.quota, q = qv && qv.show;
  return !!(q && q.over && !q.carryOn && qv.count !== "up" && !qv.free);
}
const quotaPeriod = q => t(q.period === "month" ? "quota.month" : "quota.today");
const quotaName = q => t(q.net ? "quota.nameNet" : "quota.name", { period: q.period, limit: fmtSize(q.limit), net: q.net || "" });
function quotaResets(q) {
  const d = new Date(q.resets), days = Math.round((new Date(d.toDateString()) - new Date(new Date().toDateString())) / 864e5);
  return days <= 1 && d.getHours() === 0 && d.getMinutes() === 0 ? t(days === 1 ? "quota.resetsMidnight" : "quota.resetsNow") : t("quota.resetsOn", { date: fmtDay(d) });
}
function quotaStatus() {
  const qv = S.quota, q = qv && qv.show;
  if (!q) return "";
  const tip = t(qv.free ? "quota.tipFree" : "quota.tip", { name: quotaName(q), resets: quotaResets(q) });
  if (q.over && !q.carryOn) return `<button class="cap-hit sb-btn" data-sb="quota" title="${te("bar.quotaClick", { tip })}">${te("bar.quotaUsedUp")}</button>`;
  return `<button class="sb-btn${q.warn || q.over ? " sb-warn" : ""}" data-sb="quota" title="${esc(tip)}">${ht("bar.quotaUsed", { period: quotaPeriod(q), used: fmtSize(q.used), limit: fmtSize(q.limit) })}</button>`;
}
function profileStatus() {
  const p = S.profile;
  if (!p || !p.list || !p.list.length) return "";
  const tip = t(p.byHand && p.until ? "profile.tipHand" : p.sched ? "profile.tipSched" : "profile.tip", { time: p.until ? hhmm(p.until) : "" });
  return `<button class="sb-btn" data-sb="profile" aria-haspopup="menu" title="${esc(tip)}">${ht("bar.profile", { name: nm(p.name) })}<span class="sb-caret" aria-hidden="true">▾</span></button>`;
}
function profileMenu(anchor) {
  const p = S.profile || { list: [] };
  const items = p.list.map(x => menuItem(x.name, "", () => switchProfile(x.id), { checked: x.id === p.id }));
  items.push(SEP, menuItem(t("profile.edit"), "", () => optionsDialog("conn", "o-tp")));
  showMenuAt(anchor, items);
}
async function switchProfile(id) {
  const r = await api("profile", { id });
  if (r.error) return toast(r.error, 4000);
  toast(th(r.byHand && r.until ? "profile.switchedUntil" : "profile.switched", { name: nm(r.name), time: r.until ? hhmm(r.until) : "" }));
}
function quotaDialog() {
  const qv = S.quota, q = qv && qv.show;
  if (!q) return optionsDialog("plan", "o-qsec");
  const over = q.over && !q.carryOn;
  const vars = { name: quotaName(q), used: fmtSize(q.used), count: qv.count, resets: quotaResets(q), over: q.over ? "yes" : "no" };
  const el = dialog({ title: t("quota.title"), width: 420, key: "quota", body: `<div style="white-space:pre-line">${esc(t(over ? "quota.usedUp" : "quota.status", vars))}</div>`, left: `<a href="#" data-qedit>${te("quota.change")}</a>`,
    buttons: over ? [
      { label: t("quota.carryOn"), primary: true, fn: async el => { const r = await api("quota", { id: q.id }); if (r.error) toast(r.error, 4000); el.close(); } },
      { label: t("quota.wait"), fn: el => el.close() },
    ] : [{ label: t("btn.close"), primary: true, fn: el => el.close() }] });
  $("[data-qedit]", el).onclick = e => { e.preventDefault(); el.close(); optionsDialog("plan", "o-qsec"); };
  return el;
}
function capStatus() {
  const c = S.cap;
  if (!c || !c.on) return "";
  if (c.blocked) return `<span class="cap-hit" title="${te("bar.capHitTip")}">${esc(t("bar.capHit", { wait: fmtTime(c.freeIn) || t("time.aMoment") }))}</span>`;
  return `<span class="opt" title="${te("bar.capTip", { hours: c.hours })}">${ht("bar.cap", { used: fmtSize(c.used), limit: fmtSize(c.limit) })}</span>`;
}
// powerStatus says what happens once downloads finish ("When all downloads finish", a queue's action).
function powerStatus() {
  const p = S.power, pend = p && p.pending;
  if (pend && !pend.at) return `<span class="cap-hit" title="${te("bar.thenTip", { why: pend.why })}">${esc(t("bar.then", { action: whenLabel(pend.action) }))}</span>`;
  if (p && p.whenAllDone) return `<span title="${te("bar.whenDoneTip")}">${ht("bar.whenDone", { action: whenLabel(p.whenAllDone) })}</span>`;
  return "";
}
function queueStatusText() {
  const run = S.queues.filter(qBusy);
  if (!run.length) return te("bar.queuesStopped");
  return run.length === 1 ? ht(run[0].syncing ? "bar.queueChecking" : "bar.queueRunning", { name: nm(run[0].name) }) : te("bar.queuesRunning", { n: run.length });
}
function renderAll() { renderSidebar(); renderTable(); renderStatus(); updateToolbar(); }

// ------------------------------------------------------------------ dialogs
// Every window is a native <dialog>. The ones you have to answer (Add, Properties, Options, Queues, Batch,
// Export/Import, confirms, the countdown) open modal: the browser makes the page behind inert and Tab stays
// inside. Progress windows and the site grabber are modeless, so several can sit beside the list; they
// stack and raise on click like before.
let zTop = 100, cascade = 0, dlgSeq = 0;
const modals = []; // open modal windows, newest last
// Invalid is thrown by a form check that has already put its message on the field.
class Invalid extends Error {}
const FOCUSABLE = "a[href],button,input:not([type=hidden]),select,textarea,summary,[tabindex]";
const isShown = n => n.checkVisibility ? n.checkVisibility({ visibilityProperty: true, checkVisibilityCSS: true }) : n.getClientRects().length > 0;
// Tab enters a radio group at its ticked button (or the first one), not at every button.
const radioStop = (n, root) => { if (n.checked) return true; const g = $$("input[type=radio]", root).filter(r => r.name === n.name); return !g.some(r => r.checked) && g[0] === n; };
// tabbables lists what Tab can reach inside root, in order.
const tabbables = root => $$(FOCUSABLE, root).filter(n => !n.disabled && n.tabIndex >= 0 && isShown(n) && (n.type !== "radio" || !n.name || radioStop(n, root)));
// errText turns a failed request into a sentence for a dialog's error line.
function errText(e) {
  const m = String((e && e.message) || e || "").trim();
  if (!m || /failed to fetch|networkerror|load failed/i.test(m)) return t("err.unreachable");
  return m.charAt(0).toUpperCase() + m.slice(1);
}

// field builds one label + control row. A hint and the error line sit under the control in the same column,
// so every dialog lines up the same way (the label width is --lw). id names the control the label is for
// (it is read from the control's markup when left out); group labels a set of controls instead of one.
// Other options: top (label aligned to the top), hintId, labelId, cls, attrs (extra attributes for the row),
// noRole (the control already has its own group role).
function field(label, control, o = {}) {
  const id = o.id || (control.match(/\sid="([^"]+)"/) || [])[1] || "fld" + ++dlgSeq, hid = o.hintId || id + "-h";
  if (o.hint != null && !o.group) control = control.replace(new RegExp(`(<(?:input|select|textarea)\\b[^>]*?\\sid="${id}")`), `$1 aria-describedby="${hid}"`);
  return `<div class="field${o.top ? " top" : ""}${o.cls ? " " + o.cls : ""}"${o.attrs ? " " + o.attrs : ""}>${o.group ? `<span class="lbl" id="${o.labelId || id + "-l"}">${label}</span>` : `<label for="${id}"${o.labelId ? ` id="${o.labelId}"` : ""}>${label}</label>`}`
    + `<div class="ctl"${o.group && !o.noRole ? ` role="group" aria-labelledby="${o.labelId || id + "-l"}"` : ""}>${control}</div>`
    + `${o.hint != null ? `<div class="hint" id="${hid}">${o.hint}</div>` : ""}<div class="f-err" id="${id}-e"></div></div>`;
}
// fieldError puts a message under a control and links it with aria-invalid / aria-describedby; typing in the
// control (or ticking one inside a group) clears it again.
function fieldError(ctl, msg) {
  clearFieldError(ctl);
  const row = ctl.closest(".field");
  let box = row && $(".f-err", row);
  if (!box) { // a control outside a field row (a repeated rule, say): the line goes right after its group
    box = h(`<div class="f-err" id="fe${++dlgSeq}"></div>`); box._made = true;
    (ctl.closest(".site-top,.pick,.inline,.ctl,.chk") || ctl).after(box);
  }
  box.textContent = msg;
  ctl._err = box; ctl.dataset.invalid = "1";
  const hint = row && $(".hint[id]", row);
  ctl._desc = ctl.getAttribute("aria-describedby");
  ctl.setAttribute("aria-describedby", [hint && hint.id, box.id].filter(Boolean).join(" "));
  if (ctl.matches("input,select,textarea")) { ctl.setAttribute("aria-invalid", "true"); ctl.classList.add("invalid"); }
  return ctl;
}
function clearFieldError(target) {
  const ctl = target.closest && target.closest("[data-invalid]");
  if (!ctl || !ctl._err) return;
  ctl._err.textContent = ""; if (ctl._err._made) ctl._err.remove();
  ctl.removeAttribute("aria-invalid"); ctl.classList.remove("invalid"); delete ctl.dataset.invalid;
  if (ctl._desc) ctl.setAttribute("aria-describedby", ctl._desc); else ctl.removeAttribute("aria-describedby");
  ctl._err = null;
}

// focusDialog puts focus where a window should start: the control it names, else the first text field,
// else its main button (never a destructive one), else the window itself so no key can press anything.
function focusDialog(el, want) {
  let f = want && want !== "dialog" ? $(want, el) : null;
  if (!f && want !== "dialog") {
    f = $$("input:not([readonly]):not([type=checkbox]):not([type=radio]):not([type=hidden]):not([type=button]):not([type=file]),textarea,select", $(".dlg-b", el)).find(n => !n.disabled && isShown(n))
      || $$(".dlg-f .btn.primary, .dlg-f .btn:not(.danger)", el).find(n => !n.disabled && isShown(n));
  }
  (f || el).focus({ preventScroll: true });
  if (f && f.select && f.matches("input,textarea")) f.select();
}
// restoreFocus gives focus back to what opened a window (or the list row) once it closes.
function restoreFocus(opener) {
  const top = modals[modals.length - 1];
  const ok = n => n && n.isConnected && !n.disabled && n !== document.body && isShown(n) && (!top || top.contains(n));
  if (ok(opener)) { opener.focus({ preventScroll: true }); return; }
  if (top) { focusDialog(top); return; }
  const row = $("#tbody tr.sel") || $("#tbody tr");
  if (row) { if (!row.hasAttribute("tabindex")) row.tabIndex = -1; row.focus({ preventScroll: true }); }
}
// raiseDialog brings an already open window back to the front (a modal one goes on top of the stack again).
function raiseDialog(ex) {
  ex.style.zIndex = ++zTop;
  if (ex._modal && modals[modals.length - 1] !== ex) {
    HTMLDialogElement.prototype.close.call(ex); ex.showModal();
    modals.splice(modals.indexOf(ex), 1); modals.push(ex);
  }
  if (!ex.contains(document.activeElement)) focusDialog(ex);
}

// dialog({title, width, body, buttons, left, onClose, key, modal, alert, focus}) opens a window.
//  buttons: {label, id, primary, danger, cancel, busy, fn(el, btn)}. A cancel button (Cancel, Close) works while
//   a request runs. A button with a busy label ("Saving…") is busy while fn runs: its buttons are off, it shows a
//   spinner, and a failure lands on the window's error line; so does a promise that fn returns without one.
//  key: one window per key. modal=false opens it beside the page. alert: role=alertdialog for a destructive ask.
//  focus: a selector to start on, or "dialog" to start on the window itself.
// The window has el.close(), el.error(msg), el.note(msg), el.fail(err) and el.run(button, work, busyLabel).
function dialog({ title, width = 460, body, buttons = [], left = "", onClose, key, modal = true, alert = false, focus }) {
  if (key) { const ex = document.querySelector(`dialog.dlg[data-key="${key}"]`); if (ex) { raiseDialog(ex); return ex; } }
  const n = ++dlgSeq, opener = document.activeElement;
  let ro;
  const el = h(`<dialog class="dlg${modal ? "" : " modeless"}" style="width:${width}px" tabindex="-1" aria-labelledby="dlg-t${n}"${alert ? ` role="alertdialog"` : ""}><div class="dlg-h"><h2 class="t" id="dlg-t${n}"></h2><button type="button" class="x" title="${te("btn.closeTip")}" aria-label="${te("btn.closeTip")}">✕</button></div><div class="dlg-b"></div><div class="dlg-err" role="alert"></div><div class="dlg-ok" role="status"></div><div class="dlg-f"><div class="left"></div></div></dialog>`);
  if (key) el.dataset.key = key;
  el._modal = modal;
  put($(".t", el), title); // (a string, or a message with names in it)
  const b = $(".dlg-b", el);
  if (typeof body === "string") b.innerHTML = body; else if (body) b.appendChild(body);
  $(".left", $(".dlg-f", el)).innerHTML = left;
  const msgs = (err, ok) => { put($(".dlg-err", el), err || ""); put($(".dlg-ok", el), ok || ""); };
  el.error = m => msgs(m, ""); // a problem with the whole action (not one field)
  el.note = m => msgs("", m);  // "Saved" after Apply
  el.fail = e => { if (!(e instanceof Invalid)) el.error(errText(e)); };
  // run does work with the window busy: its buttons are off (so a double click can't send it twice) and the
  // one pressed shows a spinner; a failure lands on the error line and the window stays open.
  el.run = async (btn, work, label = t("btn.working")) => {
    if (el._busy) return;
    el._busy = true; msgs();
    const was = btn && btn.innerHTML, had = document.activeElement, off = [];
    for (const x of $$(".dlg-f button", el)) if (!x.dataset.cancel && !x.disabled) { x.disabled = true; off.push(x); }
    if (btn) { btn.setAttribute("aria-busy", "true"); btn.innerHTML = `<span class="spin" aria-hidden="true"></span>${esc(label)}`; }
    try { return await work(); }
    catch (e) { el.fail(e); }
    finally {
      el._busy = false;
      if (!el.closed) {
        if (btn) { btn.removeAttribute("aria-busy"); btn.innerHTML = was; }
        off.forEach(x => x.disabled = false);
        const at = document.activeElement; // (a button that was focused lost it when it went off)
        if (off.includes(had) && (at === had || at === document.body || !el.contains(at))) had.focus({ preventScroll: true });
      }
    }
  };
  el.closed = false;
  const nativeClose = HTMLDialogElement.prototype.close;
  el.close = () => {
    if (el.closed) return;
    el.closed = true; ro.disconnect();
    const had = el.contains(document.activeElement);
    nativeClose.call(el); el.remove();
    const i = modals.indexOf(el); if (i >= 0) modals.splice(i, 1);
    if (had) restoreFocus(opener); // (a window closing in the background leaves focus where it is)
    onClose && onClose();
  };
  for (const bt of buttons) {
    const btn = h(`<button type="button" class="btn${bt.primary ? " primary" : ""}${bt.danger ? " danger" : ""}">${asHtml(bt.label)}</button>`);
    if (bt.id) btn.id = bt.id;
    if (bt.cancel) btn.dataset.cancel = "1";
    btn.onclick = () => {
      if (el._busy && !bt.cancel) return;
      if (bt.busy) return el.run(btn, () => bt.fn(el, btn), bt.busy); // a button that talks to the server is busy from the first moment
      const r = bt.fn(el, btn);
      if (r && r.then) el.run(bt.cancel ? null : btn, () => r);
    };
    $(".dlg-f", el).appendChild(btn);
  }
  if (!buttons.length && !left) $(".dlg-f", el).remove();
  $(".x", el).onclick = () => el.close();
  document.body.appendChild(el);
  let moved = false;
  el.addEventListener("focusin", () => { moved = true; });
  if (modal) { el.showModal(); modals.push(el); } else el.show();
  const r = el.getBoundingClientRect();
  // a modal sits centred (one on top of another steps down a little); progress windows cascade so several stay visible
  const off = modal ? (modals.length - 1) * 22 : (cascade++ % 6) * 22;
  el.style.left = Math.max(8, (innerWidth - r.width) / 2 + off - (modal ? 0 : 40)) + "px";
  el.style.top = Math.max(8, (innerHeight - r.height) / 2.6 + off - (modal ? 0 : 30)) + "px";
  el.style.zIndex = ++zTop;
  // start where the window says, unless its builder has already put focus somewhere (it fills in right after this)
  moved = false;
  queueMicrotask(() => { if (!moved && !el.closed) focusDialog(el, focus); });
  // keep the whole window on screen when its content grows (async dialogs fill in later)
  ro = new ResizeObserver(() => keepOnScreen(el)); ro.observe(el);
  el.addEventListener("mousedown", () => el.style.zIndex = ++zTop);
  const hd = $(".dlg-h", el);
  hd.addEventListener("mousedown", e => {
    if (e.target.closest(".x")) return;
    const x0 = e.clientX - el.offsetLeft, y0 = e.clientY - el.offsetTop;
    const mv = ev => { el.style.left = Math.min(innerWidth - 60, Math.max(-el.offsetWidth + 80, ev.clientX - x0)) + "px"; el.style.top = Math.min(innerHeight - 30, Math.max(0, ev.clientY - y0)) + "px"; };
    const up = () => { removeEventListener("mousemove", mv); removeEventListener("mouseup", up); };
    addEventListener("mousemove", mv); addEventListener("mouseup", up);
  });
  el.addEventListener("keydown", e => {
    if (e.isComposing) return;
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); el.close(); return; }
    // Enter confirms only from a text field; on a button it presses that button, on a checkbox it does nothing
    if (e.key === "Enter" && e.target.matches("input:not([type=checkbox]):not([type=radio]):not([type=button]):not([type=file]), select")) {
      const p = $$(".dlg-f .btn.primary", el).find(isShown);
      if (p && !p.disabled) { e.preventDefault(); e.stopPropagation(); p.click(); }
      return;
    }
    // the browser keeps Tab inside a modal window, but not every engine wraps from the last control to the first
    if (e.key === "Tab" && modal) {
      const tabs = tabbables(el), at = document.activeElement;
      if (!tabs.length) { e.preventDefault(); el.focus(); }
      else if (e.shiftKey && (at === tabs[0] || at === el)) { e.preventDefault(); tabs[tabs.length - 1].focus(); }
      else if (!e.shiftKey && at === tabs[tabs.length - 1]) { e.preventDefault(); tabs[0].focus(); }
    }
  });
  el.addEventListener("cancel", e => { e.preventDefault(); el.close(); }); // (the browser's own Escape / close request)
  el.addEventListener("close", () => { if (!el.open) el.close(); });
  el.addEventListener("input", e => clearFieldError(e.target));
  el.addEventListener("change", e => clearFieldError(e.target));
  return el;
}
// While a modal window is open nothing behind it may react to the keyboard (a key pressed with focus on the
// page would otherwise reach the list's shortcuts), and a window's own controls never trigger the list's
// shortcuts or the paste-a-link prompt.
document.addEventListener("keydown", e => {
  if (e.key !== "Escape" && modals.length && !(e.target.closest && e.target.closest(".dlg"))) e.stopImmediatePropagation();
}, true);
document.addEventListener("paste", e => {
  if ((modals.length || (e.target.closest && e.target.closest(".dlg"))) && !(e.target.closest && e.target.closest("input,textarea"))) e.stopImmediatePropagation();
}, true);
function keepOnScreen(el) {
  const r = el.getBoundingClientRect();
  if (r.bottom > innerHeight - 8) el.style.top = Math.max(8, innerHeight - r.height - 8) + "px";
  if (r.right > innerWidth - 8) el.style.left = Math.max(8, innerWidth - r.width - 8) + "px";
}
// confirmDialog asks before an action. A destructive one is an alert dialog with a red button and starts on
// Cancel, so a stray Enter or Space can't confirm it. If fn returns a promise the window stays up (busy) until
// it is done, and shows what went wrong.
let cfSeq = 0;
// (title and msg are strings, or messages from th() with names in them)
function confirmDialog(title, msg, okLabel, fn, opts = {}) {
  const id = "cf-msg" + ++cfSeq;
  const el = dialog({ title, width: 400, alert: !!opts.danger, focus: opts.danger ? "#cf-cancel" : undefined, body: `<div id="${id}" style="white-space:pre-line">${asHtml(msg)}</div>`, buttons: [
    { label: okLabel, primary: !opts.danger, danger: !!opts.danger, busy: t("btn.working"), fn: el => { const r = fn(); if (r && r.then) return r.then(() => el.close()); el.close(); } },
    { label: t("btn.cancel"), id: "cf-cancel", cancel: true, fn: el => el.close() },
  ] });
  el.setAttribute("aria-describedby", id);
  return el;
}

// ------------------------------------------------------------------ messages everyone can follow
// toast(msg, ms) shows a short message (a string, or a message from th()) that goes away by itself. toast(msg, { … }) can do more:
//   ms      how long it stays (default 2.6 s, 6 s with an action)
//   kind    "info" (default), "warn" or "error": the colour of its edge; an error is announced as an alert
//   action  { label, fn }: a button inside the toast (e.g. "Show"); pressing it runs fn and closes the toast
//   id      the same id again updates that toast in place instead of adding another
//   sticky  stays until dismissed or closed by the code that made it
//   quiet   not announced and not kept in Notifications (for a countdown that is redrawn every second)
//   onDismiss  called when the person closes it with × or Esc (not when the code closes it)
// It returns { el, set(text), close() }. At most 4 show at once, newest at the bottom. Each has an ×
// (and Esc while it has focus), and waits while the pointer is on it or it has focus.
const TOAST_MAX = 4;
function toast(msg, o = 2600) {
  if (typeof o === "number") o = { ms: o };
  const text = String(msg), stack = $("#toasts");
  let el = [...stack.children].find(x => o.id ? x.id === o.id : x._msg === text && !x._act);
  if (!el) { // (the same words again renew the one on screen instead of piling up)
    el = h(`<div class="toast" role="group" aria-label="${te("toast.label")}"><span class="t"></span><button class="x" title="${te("toast.dismiss")}" aria-label="${te("toast.dismissLong")}">✕</button></div>`);
    if (o.id) el.id = o.id;
    $(".x", el).onclick = () => closeToast(el, true);
    el.addEventListener("keydown", e => { if (e.key === "Escape") { e.stopPropagation(); closeToast(el, true); } });
    const arm = () => { clearTimeout(el._timer); if (el._ms) { el._from = Date.now(); el._timer = setTimeout(() => closeToast(el), el._left); } };
    const hold = () => { if (el._held) return; el._held = true; clearTimeout(el._timer); el._left = Math.max(1500, el._left - (Date.now() - el._from)); };
    const release = () => setTimeout(() => { if (!el.matches(":hover, :focus-within")) { el._held = false; arm(); } }, 0);
    el.addEventListener("mouseenter", hold); el.addEventListener("mouseleave", release);
    el.addEventListener("focusin", e => { el._back = el._back || e.relatedTarget; hold(); }); el.addEventListener("focusout", release);
    el._arm = arm;
    stack.appendChild(el);
  }
  // (the stack is a popover in the top layer, so it shows above an open modal window: raised
  // again above the newest one)
  if (stack.showPopover && (document.querySelector("dialog:modal") || !stack.matches(":popover-open"))) {
    try { if (stack.matches(":popover-open")) stack.hidePopover(); stack.showPopover(); } catch {}
  }
  el._dismiss = o.onDismiss;
  el._msg = text; put($(".t", el), msg);
  el.className = "toast" + (o.kind && o.kind !== "info" ? " " + o.kind : "");
  $$(".btn", el).forEach(b => b.remove());
  el._act = !!o.action;
  if (o.action) {
    const b = h(`<button class="btn sm"></button>`); b.textContent = o.action.label;
    b.onclick = () => { closeToast(el); o.action.fn(); };
    el.insertBefore(b, $(".x", el));
  }
  el._ms = o.sticky ? 0 : (o.ms ?? (o.action ? 6000 : 2600)); el._left = el._ms;
  if (!el._held) el._arm();
  if (!o.quiet) { note(msg, o.kind); say(text, o.kind === "error"); }
  trimToasts();
  return { el, set: m => put($(".t", el), m), close: () => closeToast(el) };
}
function closeToast(el, byPerson) {
  if (!el || !el.isConnected) return;
  clearTimeout(el._timer);
  const had = el.contains(document.activeElement);
  el.remove();
  if (had && el._back && el._back.isConnected) el._back.focus(); // (the keyboard goes back to where it was)
  if (byPerson && el._dismiss) el._dismiss();
}
// trimToasts lets the oldest leave when there are more than 4, or when the stack would reach the top of a short window
function trimToasts() {
  const stack = $("#toasts");
  while (stack.children.length > 1 && (stack.children.length > TOAST_MAX || stack.getBoundingClientRect().top < 8))
    closeToast([...stack.children].find(x => x._ms) || stack.firstElementChild);
}
addEventListener("resize", trimToasts);
// the toast stack sits just above the status bar, whatever height that has (it wraps in narrow windows)
new ResizeObserver(() => document.documentElement.style.setProperty("--sbh", $("#statusbar").offsetHeight + "px")).observe($("#statusbar"));

// ---- what a screen reader hears: two quiet regions (polite status, assertive alert) that get messages in
// batches, at most one update every few seconds each, never every progress tick. Toasts are announced
// through them; "N downloads finished", a failure, a finished queue and a used-up quota too.
const LIVE = { polite: { id: "#live-status", q: [], timer: 0, last: 0, gap: 2500 }, alert: { id: "#live-alert", q: [], timer: 0, last: 0, gap: 4000 } };
function say(msg, urgent) {
  msg = String(msg);
  const L = urgent ? LIVE.alert : LIVE.polite;
  if (!L.q.includes(msg)) L.q.push(msg);
  if (!L.timer) L.timer = setTimeout(() => {
    L.timer = 0; L.last = Date.now();
    const q = L.q.splice(0), el = $(L.id);
    if (!q.length || !el) return;
    el.textContent = ""; // (emptied first, so the same words again are read again)
    setTimeout(() => { el.textContent = q.length > 3 ? t("live.more", { first: q.slice(0, 2).join(". "), n: q.length - 2 }) : q.join(". "); }, 60);
  }, Math.max(400, L.last + L.gap - Date.now()));
}
// Downloads that finish together are announced and listed together.
const FINISHED = { list: [], first: 0, timer: 0 };
function downloadFinished(id) {
  const d = S.byId.get(id), f = FINISHED;
  f.list.push({ id, name: d ? dname(d) : t("n.aDownload") });
  if (!f.first) f.first = Date.now();
  clearTimeout(f.timer);
  f.timer = setTimeout(() => {
    const l = f.list.splice(0); f.first = 0;
    const text = th("n.finished", { n: l.length, name: nm(l[0].name) });
    note(text); say(text);
    // (without the "finished" window, a quiet message says so)
    if (!(S.settings?.showCompleteDialog ?? true)) toast(text, { quiet: true, action: l.length === 1 ? { label: t("btn.show"), fn: () => showDownload(l[0].id) } : undefined });
  }, Date.now() - f.first > 4000 ? 0 : 1500);
}
// showDownload selects a download in the list (switching the view if it isn't in this one).
function showDownload(id) {
  if (!S.byId.has(id)) return;
  if (!visible().some(d => d.id === id)) { S.filter = "all"; S.search = ""; $("#search").value = ""; saveUI(); }
  S.selected = new Set([id]); S.anchor = id; LIST.focus = id;
  renderAll();
  showRow(listIds().indexOf(id)); // (rows exist only near the view: scroll by position, then draw)
  renderRows();
}

// ---- Notifications: the last 50 messages with their times, newest first (the bell in the status bar)
const NOTES = [];
let noteSeq = 0, notesSeen = 0, notesEl = null;
function note(msg, kind) {
  NOTES.push({ n: ++noteSeq, t: Date.now(), msg: String(msg), html: asHtml(msg), kind: kind || "info" });
  if (NOTES.length > 50) NOTES.shift();
  drawNotes();
}
function updateBell() {
  const b = $("#sb-bell"), n = $(".n", b), fresh = NOTES.filter(x => x.n > notesSeen);
  n.hidden = !fresh.length; n.textContent = fresh.length > 9 ? "9+" : fresh.length;
  n.classList.toggle("bad", fresh.some(x => x.kind === "error"));
  b.setAttribute("aria-label", fresh.length ? t("notes.bellNew", { n: fresh.length }) : t("notes.title"));
  b.title = fresh.length ? t("notes.bellTip", { n: fresh.length }) : t("notes.title");
}
const noteTime = ms => { const d = new Date(ms), tm = fmtClock(d); return d.toDateString() === new Date().toDateString() ? tm : fmtDay(d) + " " + tm; };
function drawNotes() {
  if (notesEl) notesSeen = noteSeq; // (open: everything in it counts as seen)
  updateBell();
  if (!notesEl) return;
  $("#notes-body", notesEl).innerHTML = NOTES.length
    ? `<ol class="notes">${[...NOTES].reverse().map(x => `<li class="note${x.kind === "error" ? " error" : ""}"><time datetime="${new Date(x.t).toISOString()}">${noteTime(x.t)}</time><span>${x.html}</span></li>`).join("")}</ol>`
    : `<div class="note-empty">${te("notes.empty")}</div>`;
  $("#notes-clear", notesEl).disabled = !NOTES.length;
}
$("#sb-bell").addEventListener("click", () => notesDialog());
function notesDialog() {
  notesEl = dialog({ title: t("notes.title"), width: 440, key: "notes", body: `<div id="notes-body"></div>`,
    buttons: [{ label: t("notes.clear"), id: "notes-clear", fn: () => { NOTES.length = 0; drawNotes(); } }, { label: t("btn.close"), primary: true, fn: el => el.close() }],
    onClose: () => { notesEl = null; } });
  drawNotes();
}

// progressBar returns a bar that screen readers read as a progress bar, with its value and the time left
// ("42% · 3 min left"). cls is its look ("pb" in the list, "bigbar" in the progress windows) and fill the
// class of the inner bar that setProgress updates later.
function progressBar(pct, text, { cls = "pb", fill = "", label = "" } = {}) {
  const v = Math.max(0, Math.min(100, +pct || 0));
  return `<div class="${cls}" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${Math.floor(v)}" aria-valuetext="${esc(text || fmtPct(Math.floor(v), 0))}"${label ? ` aria-label="${esc(label)}"` : ""}><i${fill ? ` class="${fill}"` : ""} style="width:${v.toFixed(2)}%"></i></div>`;
}
function setProgress(fillEl, pct, text) {
  const v = Math.max(0, Math.min(100, +pct || 0)), bar = fillEl.parentElement;
  fillEl.style.width = v + "%";
  bar.setAttribute("aria-valuenow", Math.floor(v));
  bar.setAttribute("aria-valuetext", text || fmtPct(Math.floor(v), 0));
}
const progressText = d => { const left = d.status === "downloading" && !d.stage ? fmtTime(etaOf(d)) : ""; return t(left ? "pb.withLeft" : "pb.value", { pct: fmtPct(Math.floor(pct(d)), 0), left }); };

// ---- Add URL
function headerFields(prefix = "") {
  return `<details class="fold"><summary>${te("dlg.advanced")}</summary><div class="fold-body">
      ${field(te("f.user"), `<input class="inp" id="${prefix}user" autocomplete="off">`)}
      ${field(te("f.password"), `<input class="inp" id="${prefix}pass" type="password" autocomplete="off">`)}
      ${field(te("f.referer"), `<input class="inp" id="${prefix}ref" spellcheck="false">`)}
      ${field(te("f.cookie"), `<input class="inp" id="${prefix}cookie" spellcheck="false">`)}
    </div></details>`;
}
function readHeaders(el, prefix = "") {
  const hd = {};
  const u = $("#" + prefix + "user", el)?.value, p = $("#" + prefix + "pass", el)?.value;
  if (u) hd["Authorization"] = "Basic " + btoa(unescape(encodeURIComponent(u + ":" + p)));
  const r = $("#" + prefix + "ref", el)?.value.trim(); if (r) hd["Referer"] = r;
  const c = $("#" + prefix + "cookie", el)?.value.trim(); if (c) hd["Cookie"] = c;
  return hd;
}
async function clipText() {
  try { const text = await navigator.clipboard.readText(); if (text) return text; } catch {}
  try { return (await api("clipboard")).text || ""; } catch { return ""; }
}
async function addUrlDialog(prefill) {
  if (prefill === undefined) { const text = (await clipText()).trim(); prefill = LINK_RE.test(text) ? text : ""; }
  const el = dialog({
    title: t("dlg.addLink.title"), width: 520, key: "addurl",
    body: `${field(te("f.link"), `<input class="inp" id="u" spellcheck="false" placeholder="${te("dlg.addLink.placeholder")}">`)}${headerFields()}`,
    left: `<button class="btn ico" id="u-torrent">${I.folderOpen}<span>${te("menu.openTorrent")}</span></button>`,
    buttons: [
      { label: t("btn.ok"), primary: true, fn: el => { const u = $("#u", el).value.trim(); if (!LINK_RE.test(u)) { fieldError($("#u", el), t("dlg.addLink.bad")); $("#u", el).focus(); return; } const hd = readHeaders(el); el.close(); fileInfoDialog(u, hd); } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
  });
  $("#u", el).value = prefill || "";
  $("#u-torrent", el).onclick = () => { el.close(); openTorrentFile(); };
}
// openTorrentFile lets you pick .torrent files and opens the Add dialog for each.
function openTorrentFile(files) {
  if (!files) {
    const inp = h(`<input type="file" accept=".torrent,application/x-bittorrent" multiple hidden>`);
    document.body.appendChild(inp);
    inp.onchange = () => { openTorrentFile([...inp.files]); inp.remove(); };
    inp.click();
    return;
  }
  for (const f of files) {
    if (f.size > 20 << 20) { toast(th("n.torrentTooBig", { name: nm(f.name) })); continue; }
    const rd = new FileReader();
    rd.onload = async () => {
      const r = await api("torrent/open", { data: String(rd.result).split(",")[1] || "" }).catch(e => ({ error: String(e.message || e) }));
      if (r.error) toast(th("n.nameError", { name: nm(f.name), error: r.error }), 5000); else fileInfoDialog(r.url);
    };
    rd.readAsDataURL(f);
  }
}
// catOptions lists the categories as <option>s (the value stays the category's own name, whatever language it shows in).
const catOptions = sel => CATS.map(c => `<option value="${c}"${c === sel ? " selected" : ""}>${esc(catName(c))}</option>`).join("");
// pickControl is a folder field with its Browse button.
const pickControl = (dirId, browseId) => `<div class="pick"><input class="inp" id="${dirId}" spellcheck="false"><button class="btn browse" id="${browseId}" title="${te("btn.chooseFolder")}">${I.folderOpen}<span>${te("btn.browse")}</span></button></div>`;

// ---- A video stream's tracks in the Add dialog: the quality (codec, frame rate, HDR), the audio
// language and the subtitles. The server lists what the stream offers (probe) and does the same
// sums when it downloads (tracks.go, streamout.go); these pick the defaults and name the file.
const canonLang = tag => { try { return Intl.getCanonicalLocales(String(tag || "").replace(/_/g, "-"))[0] || ""; } catch { return ""; } };
const langOf = tag => canonLang(tag).split("-")[0].toLowerCase();
let langNames = null;
// langName names a language in the window's language ("" when the stream doesn't say).
function langName(tag) {
  const c = canonLang(tag);
  if (!c || /^(und|zxx|mis|mul|q[a-t][a-z])$/i.test(c)) return "";
  try { langNames = langNames || new Intl.DisplayNames(LOCALE, { type: "language" }); return cap1(langNames.of(c) || tag); } catch { return tag; }
}
// codecFam names a codec string's format, as codecFamily in tracks.go does.
function codecFam(c) {
  const p = String(c || "").trim().toLowerCase().split(".");
  const fam = { avc1: "h264", avc3: "h264", hvc1: "hevc", hev1: "hevc", dvh1: "dv", dvhe: "dv", dva1: "dv", dvav: "dv", av01: "av1", vp09: "vp9", vp9: "vp9", vp08: "vp8", vp8: "vp8",
    "ac-3": "ac3", "ec-3": "eac3", opus: "opus", vorbis: "vorbis", flac: "flac", alac: "alac", dtsc: "dts", dtse: "dts", dtsh: "dts", dtsl: "dts", "ac-4": "ac4", mhm1: "mpegh", mha1: "mpegh", wvtt: "text", stpp: "text" }[p[0]];
  if (p[0] === "mp4a") return ["69", "6b"].includes(p[1]) || p.slice(1).join(".") === "40.34" ? "mp3" : "aac";
  return fam || "";
}
const VIDEO_FAMS = new Set(["h264", "hevc", "dv", "av1", "vp9", "vp8"]);
const WEBM_FAMS = new Set(["vp8", "vp9", "av1", "opus", "vorbis"]);
const codecsOf = s => String(s || "").split(",").map(x => x.trim()).filter(x => x && codecFam(x) !== "text");
const codecName = f => f && MESSAGES.en["codec." + f] ? t("codec." + f) : String(f || "").toUpperCase();
const isHDR = v => /^(PQ|HLG)$/i.test(v.videoRange || "") || codecsOf(v.codecs).some(c => {
  const f = codecFam(c), p = c.split(".");
  return f === "dv" || (f === "vp9" && ["16", "18"].includes(p[6])) || (f === "av1" && ["16", "18"].includes(p[7]));
});
// variantLabel says a quality in plain words: "1080p · 60 fps · H.264 · 6.2 Mbps", "4K · HDR · HEVC · 16 Mbps".
function variantLabel(v) {
  const [w, h] = String(v.resolution || "").split("x").map(Number), p = w && h ? Math.min(w, h) : 0;
  const parts = [!p ? t("fi.stream") : p >= 4000 ? t("q.8k") : p >= 2000 ? t("q.4k") : t("q.res", { p: String(p) })];
  if (v.frameRate > 31) parts.push(t("q.fps", { n: Math.round(v.frameRate) }));
  if (isHDR(v)) parts.push(t("q.hdr"));
  const vc = codecsOf(v.codecs).map(codecFam).find(f => VIDEO_FAMS.has(f));
  if (vc) parts.push(codecName(vc));
  if (v.avgBandwidth || v.bandwidth) parts.push(fmtMbps(v.avgBandwidth || v.bandwidth));
  return parts.join(" · ");
}
const chLabel = c => { const n = parseInt(c, 10); return /JOC/i.test(c) ? t("ch.atmos") : n === 1 ? t("ch.mono") : n === 2 ? t("ch.stereo") : n === 6 ? "5.1" : n === 8 ? "7.1" : n > 0 ? t("ch.n", { n }) : ""; };
// nativeName is a language's name in itself ("Deutsch"), to tell when a track's name only says that.
const nativeName = tag => { try { const c = canonLang(tag); return c ? new Intl.DisplayNames([c], { type: "language" }).of(c) || "" : ""; } catch { return ""; } };
// trackLabel says an audio or subtitle track: "English · 5.1 · Dolby Digital Plus · audio description".
// The stream's own name for it is added when it says more than the language ("English (commentary)"
// stands instead of "English").
function trackLabel(a, withCodec) {
  const ln = langName(a.lang), nm = String(a.name || "").trim(), low = nm.toLowerCase();
  const same = [ln, nativeName(a.lang), a.lang, langOf(a.lang)].some(x => x && x.toLowerCase() === low);
  const parts = [ln && nm && !same && low.includes(ln.toLowerCase()) ? nm : ln || nm || t("lang.und")];
  if (ln && nm && !same && parts[0] !== nm) parts.push(nm);
  if (a.channels) parts.push(chLabel(a.channels));
  if (withCodec && a.codec) parts.push(codecName(codecFam(a.codec)));
  if (a.desc) parts.push(t("aud.desc"));
  if (a.forced) parts.push(t("sub.forced"));
  if (a.sdh) parts.push(t("sub.sdh"));
  return parts.filter(Boolean).join(" · ");
}
// userLangPick finds the track in the window's language (not audio description, not forced subtitles).
function userLangPick(list) {
  const want = canonLang(navigator.language || ""), base = langOf(want);
  const ok = list.filter(a => !a.desc && !a.forced && base && langOf(a.lang) === base);
  return ok.find(a => canonLang(a.lang).toLowerCase() === want.toLowerCase()) || ok.find(a => a.default) || ok[0] || null;
}
// streamExt is the file type the tracks make (streamout.go: pickContainer).
function streamExt(tracks, audioOnly) {
  let mp4 = true, webm = true, nA = 0, opus = false;
  for (const x of tracks) {
    if (x.webm || x.fam === "vorbis") mp4 = false;
    if (!WEBM_FAMS.has(x.fam)) webm = false;
    if (!x.video) { nA++; opus = opus || x.fam === "opus"; }
  }
  if (audioOnly) return nA === 1 && opus ? ".opus" : mp4 ? ".m4a" : ".mka";
  return mp4 ? ".mp4" : webm ? ".webm" : ".mkv";
}
const STREAM_EXTS = /\.(mp4|m4a|webm|weba|mkv|mka|opus|ogg|ts)$/i;
// streamFields is the markup of a stream's pickers (quality, audio, subtitles, ad breaks).
function streamFields(info) {
  const dash = info.format === "DASH", seen = new Set();
  const quals = (info.variants || []).filter(v => dash || !seen.has(v.url) && seen.add(v.url)); // (HLS: one per video; its audio groups are the audio choices)
  const subs = info.subs || [], ads = info.ads || [];
  let html = "";
  if (quals.length) html += field(te("f.quality"), `<select class="inp" id="var">${quals.map((v, i) => `<option value="${i}">${esc(variantLabel(v))}</option>`).join("")}<option value="audio">${te("fi.audioOnly")}</option></select>`);
  html += field(te("f.audio"), `<select class="inp" id="aud"></select><label class="chk" style="margin:8px 0 0"><input type="checkbox" id="aud-all"> ${te("aud.all")}</label>`,
    { top: true, attrs: `id="aud-row" hidden`, hint: te("aud.allHint"), hintId: "aud-h" });
  if (subs.length) html += field(te("f.subs"), `<select class="inp" id="sub"><option value="">${te("sub.none")}</option>${subs.map((s, i) => `<option value="${i}">${esc(trackLabel(s, false))}</option>`).join("")}${subs.length > 1 ? `<option value="all">${te("sub.all", { n: subs.length })}</option>` : ""}</select>`, { hint: te("sub.hint") });
  if (ads.length) html += field(te("f.ads"), `<label class="chk" style="margin:0"><input type="checkbox" id="noads"> ${te("ads.drop")}</label>`,
    { group: true, id: "noads-g", hint: te("ads.hint", { n: ads.length, dur: fmtDur(ads.reduce((s, a) => s + a.dur, 0)) }) });
  return { html, quals };
}
// streamWire sets the pickers' defaults and keeps the audio list, the file name and the "Saved as"
// line in step with the choices. pick() is what the add request gets.
function streamWire(el, info, quals) {
  const dash = info.format === "DASH", audios = info.audios || [], subs = info.subs || [];
  const qSel = $("#var", el), aSel = $("#aud", el), aAll = $("#aud-all", el), sSel = $("#sub", el), nameIn = $("#name", el);
  const ch = info.chosen || {};
  if (qSel) { const i = quals.findIndex(v => v.url === ch.url && (dash || v.bandwidth === ch.bandwidth)), j = quals.findIndex(v => v.url === ch.url); qSel.value = String(i >= 0 ? i : Math.max(j, 0)); }
  const qual = () => qSel && qSel.value !== "audio" ? quals[+qSel.value] : quals.find(v => v.url === ch.url) || quals[0] || ch;
  // HLS: the audio of a video is its renditions, in every group a variant of that video has
  const audioFor = v => {
    if (dash) return audios;
    const groups = new Set((info.variants || []).filter(x => x.url === (v && v.url)).map(x => x.audioGroup).filter(Boolean));
    return audios.filter(a => groups.has(a.group));
  };
  let list = [];
  const drawAudio = keep => {
    list = audioFor(qual());
    const codecs = new Set(list.map(a => codecFam(a.codec))).size > 1;
    aSel.innerHTML = list.map((a, i) => `<option value="${i}">${esc(trackLabel(a, codecs))}</option>`).join("");
    const k = keep !== undefined ? keep : (userLangPick(list) || list.find(a => a.key === info.audioKey) || list.find(a => a.default && !a.desc) || list.find(a => a.builtin) || list[0] || {}).key;
    const i = list.findIndex(a => a.key === k);
    aSel.value = String(i >= 0 ? i : 0);
    $("#aud-row", el).hidden = list.length < 2;
  };
  const audio = () => list[+aSel.value] || null;
  // the quality entry that goes with the chosen audio (HLS: the same video in the audio's group)
  const variant = () => {
    const v = qual(), a = audio();
    if (dash || !a || !v) return v;
    return (info.variants || []).find(x => x.url === v.url && x.audioGroup === a.group) || v;
  };
  const sound = () => qSel && qSel.value === "audio";
  const ext = () => {
    const v = variant(), a = audio(), tr = [];
    if (!sound() && v) for (const c of codecsOf(v.codecs).filter(c => VIDEO_FAMS.has(codecFam(c)))) tr.push({ fam: codecFam(c), webm: !!v.webm, video: true });
    if (!sound() && v && !codecsOf(v.codecs).some(c => VIDEO_FAMS.has(codecFam(c))) && (v.resolution || !dash)) tr.push({ fam: v.webm ? "vp9" : "h264", webm: !!v.webm, video: true });
    const ac = v ? codecsOf(v.codecs).filter(c => !VIDEO_FAMS.has(codecFam(c))) : [];
    const addA = x => tr.push({ fam: codecFam(x.codec) || (x.webm ? "" : "aac"), webm: !!x.webm, video: false });
    if (a && a.codec && !a.builtin) addA(a); else if (ac.length) tr.push({ fam: codecFam(ac[0]), webm: false, video: false }); else if (a || !dash) tr.push({ fam: "aac", webm: false, video: false });
    if (aAll.checked && list.length > 1) for (const x of list) if (x !== a && !x.builtin) addA(x);
    return streamExt(tr, sound() || !tr.some(x => x.video));
  };
  const SAVED = { ".mp4": info.separateAudio || audios.length > 1 ? "fi.savedMerged" : info.fmp4 ? "fi.savedMp4" : "fi.savedRepack", ".webm": "fi.savedWebm", ".mkv": "fi.savedMkv", ".m4a": "fi.savedM4a", ".opus": "fi.savedOpus", ".mka": "fi.savedMka" };
  const update = () => {
    const e = ext(), saved = $("#fi-saved", el);
    if (STREAM_EXTS.test(nameIn.value) && !/\.ts$/i.test(nameIn.value) || !/\.[a-z0-9]{2,4}$/i.test(nameIn.value)) nameIn.value = nameIn.value.replace(STREAM_EXTS, "") + e;
    if (saved && !/\.ts$/i.test(nameIn.value)) saved.innerHTML = ht(SAVED[e] || "fi.savedRepack");
  };
  drawAudio();
  if (sSel) { const s = userLangPick(subs); sSel.value = s ? String(subs.indexOf(s)) : ""; }
  if (qSel) qSel.addEventListener("change", () => { const k = (audio() || {}).key; drawAudio(k); update(); });
  aSel.addEventListener("change", update);
  aAll.addEventListener("change", update);
  update();
  return {
    pick() {
      const v = variant() || {}, a = audio(), s = sSel && sSel.value !== "" && sSel.value !== "all" ? subs[+sSel.value] : null;
      const p = { variant: v.bandwidth || 0, varRes: v.resolution || "", varCodecs: v.codecs || "", varFps: v.frameRate || 0, audioOnly: sound(), noAds: !!$("#noads", el)?.checked };
      if (a && list.length > 1) Object.assign(p, { audioKey: a.key, audioLang: a.lang || "", audioName: a.name || "", audioAll: aAll.checked });
      if (s) Object.assign(p, { subKey: s.key, subLang: s.lang || "", subName: s.name || "" });
      if (sSel && sSel.value === "all") p.subAll = true;
      return p;
    },
  };
}
async function fileInfoDialog(url, headers = {}, presetName = "", onDone, title = "", preset = {}) {
  // early start: the app may begin downloading as soon as it has the file's details
  let earlyId = "", committed = false, keepTimer = 0, added = false;
  // a download from the browser: its own request (a form's POST), what its reply said (so the app doesn't
  // ask the server again), a private window; closing the dialog without adding it hands it back
  const req = { method: preset.method || "", body: preset.body || "", known: preset.known || null, private: !!preset.private };
  let torrent = null; // a torrent's own Start/Later/close handling
  const el = dialog({
    title: t("fi.title"), width: 560,
    body: `<div id="fi"><span class="spin"></span>${te("fi.requesting")}</div>`,
    buttons: [
      { label: t("fi.later"), id: "later", busy: t("fi.adding"), fn: el => go(el, false) },
      { label: t("fi.start"), primary: true, id: "start", busy: t("fi.starting"), fn: el => go(el, true) },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
    onClose: () => {
      clearInterval(keepTimer);
      torrent && torrent.close();
      if (earlyId && !committed) api("early", { action: "cancel", id: earlyId }).catch(() => {});
      if (preset.taken) api("browser/closed", { id: preset.promptId, added: added || committed }).catch(() => {});
      onDone && onDone();
    },
  });
  $("#later", el).disabled = $("#start", el).disabled = true;
  let info;
  try { info = await api("probe", { url, headers, title, early: true, ...req }); } catch (e) { info = { error: String(e.message || e) }; }
  if (!el.isConnected) { if (info.early) api("early", { action: "cancel", id: info.early }).catch(() => {}); return; }
  if (info.early) {
    earlyId = info.early;
    keepTimer = setInterval(() => api("early", { action: "keep", id: earlyId }).catch(() => {}), 15000);
  }
  if (info.kind === "torrent") { torrent = torrentDialog(el, info.url || url, info, preset); added = true; return; } // (its own dialog; never handed back)
  if (info.protected) { $("#fi", el).outerHTML = `<div class="err-box">${esc(cap1(info.error))}.</div>`; return; } // YouTube, DRM services
  const st = S.settings || await api("settings");
  const name = presetName || info.name || decodeURIComponent((url.split("?")[0].split("/").pop() || "download"));
  const cat = info.category || "General";
  const dir = preset.dir || info.dir || st.downloadDir;
  const hls = info.kind === "hls";
  const blocked = hls && !!info.error; // DRM (or a live DASH stream): can't be downloaded at all
  const live = hls && info.live && !blocked; // a live stream: recorded as it plays
  const sf = hls && !blocked ? streamFields(info) : null, quality = sf ? sf.html : ""; // quality, audio, subtitles, ad breaks
  const liveRows = !live ? "" : `
    ${info.event ? field(te("f.record"), `<select class="inp" id="lv-from"><option value="now">${te("fi.liveNow")}</option><option value="start">${te("fi.liveStart")}</option></select>`) : ""}
    ${field(te("f.stopAfter"), `<input class="inp narrow" id="lv-time" spellcheck="false" inputmode="numeric" placeholder="${te("fi.stopAfterPh")}" aria-label="${te("fi.stopAfterAria")}">`, { hint: te("fi.stopAfterHint") })}`;
  const fold = info.folder, ml = info.metalink || fold, mlFiles = ml ? ml.files : []; // (a shared folder lists its files like a metalink)
  const match = info.matches; // the same file as a download whose link expired
  const matchPct = match && (() => { const q = match.size > 0 ? match.done / match.size * 100 : 0; return fmtPct(q < 10 ? q : Math.floor(q), q < 10 ? 1 : 0); })();
  const matchBox = match ? `<div class="info-box" id="fi-match"><div>${ht("fi.match", { name: nm(match.name), pct: matchPct })}</div><button class="btn sm" id="fi-renew">${te("fi.renew")}</button></div>` : "";
  const errBox = !info.error ? "" : blocked
    ? `<div class="err-box">${esc(info.error.charAt(0).toUpperCase() + info.error.slice(1))}.</div>`
    : info.challenge // bot protection: the browser can get past it, then the extension hands the file over
    ? `<div class="err-box err-act"><div>${te("fi.challenge")}</div><button class="btn sm" id="fi-browse">${te("ctx.openInBrowser")}</button></div>`
    : `<div class="err-box">${ht("fi.readFail", { error: info.error })}</div>`;
  const facts = fold ? folderFacts(info, st) : hls ? `
      <div>${te("fact.type")}</div><div class="wrap">${ht(live ? (info.event ? "fi.eventType" : "fi.liveType") : info.fmp4 && info.format !== "DASH" ? "fi.hlsTypeMp4" : "fi.hlsType", { format: info.format || "HLS" })}</div>
      <div>${te("fact.length")}</div><div>${live ? te("fi.liveLength") : info.duration ? fmtDur(info.duration) : "—"}</div>
      ${live ? "" : `<div>${te("fact.pieces")}</div><div>${ht("fi.hlsPieces", { n: info.segments || "—", conns: Math.min(12, st.conns) })}</div>`}
      <div>${te("fact.savedAs")}</div><div class="wrap" id="fi-saved">${ht(info.separateAudio ? "fi.savedMerged" : /\.ts$/i.test(name) ? "fi.savedTs" : info.fmp4 ? "fi.savedMp4" : "fi.savedRepack")}</div>
      ${live ? `<div>${te("fact.recording")}</div><div class="wrap">${te("fi.liveSaved")}</div>` : ""}` : `
      ${ml ? `<div>${te("fact.type")}</div><div>${ht(ml.total > 0 ? "fi.metalinkSize" : "fi.metalink", { n: mlFiles.length, size: fmtSize(ml.total) })}</div>
      ${mlFacts(mlFiles)}<div>${te("fact.resume")}</div>` : `
      <div>${te("fact.size")}</div><div>${info.size > 0 ? ht("fi.sizeBytes", { size: fmtSize(info.size), bytes: info.size }) : te("size.unknown")}</div>
      <div>${te("fact.resume")}</div>`}<div>${info.error ? "—" : ht(info.resumable ? "fi.resumeYes" : "fi.resumeNo", { n: st.conns })}</div>
      ${info.mime && !ml ? `<div>${te("fact.type")}</div><div>${esc(info.mime)}</div>` : ""}
      ${info.service ? `<div>${te("fact.from")}</div><div>${ht("fi.fromShared", { service: info.service })}</div>` : ""}
      ${info.serverMirrors ? `<div>${te("fact.mirrors")}</div><div>${ht("fi.mirrorsMore", { n: info.serverMirrors })}</div>` : ""}
      ${info.checksum ? `<div>${te("fact.checksum")}</div><div>${ht("fi.sumSent", { algo: ALGO[info.checksum.algo] })}</div>` : ""}
      ${info.early ? `<div>${te("fact.downloaded")}</div><div id="fi-early" class="wrap">${te("fi.starting")}</div>` : ""}
      ${S.plan && S.plan.on ? `<div>${te("fact.freeHours")}</div><div class="wrap">${esc(cap1(planWindowText(S.plan)))}</div>` : ""}`;
  const multi = mlFiles.length > 1;
  const fileChoice = multi ? field(te("f.download"), `<select class="inp" id="mlf"><option value="-1">${te(ml.total > 0 ? "fi.allFilesSize" : "fi.allFiles", { n: mlFiles.length, size: fmtSize(ml.total) })}</option>${mlFiles.map((f, i) => `<option value="${i}">${esc(f.name)}${f.size > 0 ? ` (${fmtSize(f.size)})` : ""}</option>`).join("")}</select>`) : "";
  const more = hls || ml ? "" : `<details class="fold" id="fi-more"><summary>${te("fi.more")}</summary><div class="fold-body">
      ${field(te("f.checksum"), `<input class="inp" id="sum" spellcheck="false" placeholder="${te("fi.sumPlaceholder")}">`, { hint: te("fi.sumHint") })}
      ${field(te("f.mirrors"), `<textarea class="inp" id="mirrors" rows="3" spellcheck="false" placeholder="https://mirror.example.org/file.iso"></textarea>`, { top: true, hint: te("fi.mirrorsHint") })}
    </div></details>`;
  const privBox = (req.private ? `<div class="info-box"><div>${te("fi.private")}</div></div>` : "")
    + (fold && fold.more ? `<div class="info-box"><div>${te("fi.folderMore", { n: mlFiles.length })}</div></div>` : "");
  $("#fi", el).outerHTML = `
    ${matchBox}${errBox}${privBox}
    ${field(te("f.link"), `<input class="inp" id="url" spellcheck="false">`)}
    ${fileChoice}
    ${quality}
    ${liveRows}
    ${field(te("f.category"), `<select class="inp" id="cat">${catOptions(cat)}</select>`)}
    ${field(te("f.saveTo"), pickControl("dir", "browse"))}
    ${field(te("f.fileName"), `<input class="inp" id="name" spellcheck="false">`)}
    ${field(te("f.desc"), `<input class="inp" id="desc">`)}
    ${field(te("f.laterInto"), `<select class="inp" id="qsel">${S.queues.map(q => `<option value="${q.id}">${esc(q.name)}</option>`).join("")}</select>`)}
    ${more}
    <div class="kv" style="margin-top:12px">${facts}
    </div>`;
  $("#url", el).value = url; $("#dir", el).value = dir; $("#name", el).value = name;
  const stream = sf ? streamWire(el, info, sf.quals) : null;
  if (multi) $("#mlf", el).onchange = () => {
    const i = +$("#mlf", el).value, nameIn = $("#name", el), cs = $("#cat", el);
    nameIn.disabled = cs.disabled = i < 0; nameIn.value = i < 0 ? t("fi.eachOwnName") : mlFiles[i].name.split("/").pop();
    let auto = $("option[value='']", cs);
    if (i < 0) {
      if (!auto) { auto = h(`<option value="">${te("fi.byType")}</option>`); cs.prepend(auto); }
      cs.value = ""; $("#dir", el).value = st.downloadDir;
    } else {
      if (auto) auto.remove();
      cs.value = catFor(mlFiles[i].name); cs.onchange();
    }
  };
  if (multi) $("#mlf", el).onchange();
  if (fold && multi) $("#mlf", el).addEventListener("change", () => { $("#fi-fsaved", el).innerHTML = folderSaved(fold, +$("#mlf", el).value < 0); });
  const sumIn = $("#sum", el);
  if (sumIn) sumIn.oninput = () => {
    const g = sumGuess(sumIn.value), hint = $("#sum-h", el);
    hint.classList.toggle("bad", g === null); sumIn.classList.toggle("invalid", g === null);
    hint.textContent = g === "" ? t("fi.sumHint") : g === null ? t("fi.sumBad") : t("fi.sumGuess", { algo: cap1(sumGuessName(g)) });
  };
  if (info.challenge) $("#fi-browse", el).onclick = () => { if (!preset.taken) api("openurl", { url }); el.close(); }; // (taken from the browser: closing hands it back)
  if (match) $("#fi-renew", el).onclick = e => el.run(e.currentTarget, async () => {
    const r = await api("renew", { id: match.id, url: $("#url", el).value.trim(), headers });
    if (r.error) throw new Error(r.error);
    added = true; el.close();
  }, t("fi.switching"));
  if (earlyId) {
    el._draw = () => {
      const d = S.early && S.early.get(earlyId), out = $("#fi-early", el);
      if (!d || !out) return;
      const p = d.size > 0 ? Math.min(100, d.done / d.size * 100) : 0;
      out.innerHTML = d.status === "completed" ? ht("fi.earlyDone")
        : d.status === "error" ? ht("fi.earlyError", { error: d.error || t("fi.cantStart") })
        : ht(d.speed > 0 ? "fi.earlyProgressSpeed" : "fi.earlyProgress", { done: fmtSize(d.done), pct: fmtPct(p, 0), speed: fmtSpeed(d.speed) });
    };
    el._draw();
  }
  let tq = preset.queueId || queueTarget();
  if (!preset.queueId && S.plan && S.plan.on && !S.plan.in && S.queues.some(q => q.id === PLAN_Q)) tq = PLAN_Q; // outside free hours: suggest them
  if (tq && S.queues.some(q => q.id === tq)) $("#qsel", el).value = tq;
  const laterLabel = () => { $("#later", el).textContent = t($("#qsel", el).value === PLAN_Q ? "fi.laterFree" : "fi.later"); };
  $("#qsel", el).addEventListener("change", laterLabel); laterLabel();
  $("#cat", el).onchange = () => {
    const c = $("#cat", el).value;
    $("#dir", el).value = st.categoryFolders && c !== "General" ? joinPath(st.downloadDir, c) : st.downloadDir;
  };
  $("#browse", el).onclick = () => pickFolder($("#dir", el));
  $("#later", el).disabled = $("#start", el).disabled = blocked;
  if (!blocked) $("#start", el).focus();
  async function go(el, start) {
    if (torrent) return torrent.go(start);
    const qid = $("#qsel", el).value;
    const pick = { name: $("#name", el).disabled ? "" : $("#name", el).value.trim(), dir: $("#dir", el).value.trim(), desc: $("#desc", el).value,
      checksum: $("#sum", el)?.value.trim() || "", mirrors: ($("#mirrors", el)?.value || "").split(/\s+/).filter(Boolean) };
    const badSum = msg => { $("#fi-more", el).open = true; fieldError($("#sum", el), msg); $("#sum", el).focus(); };
    if (pick.checksum && sumGuess(pick.checksum) === null) return badSum(t("fi.sumBad"));
    if (ml) Object.assign(pick, { meta: true, metaFile: multi ? +$("#mlf", el).value : 0 });
    if (ml && pick.metaFile < 0 && pick.dir === st.downloadDir && st.categoryFolders) pick.dir = ""; // each file goes to its own type's folder
    if (live) { // how much to record
      const lt = $("#lv-time", el), secs = lt.value.trim() ? hmSecs(lt.value) : 0;
      if (secs < 0) { fieldError(lt, t("fi.stopAfterBad")); lt.focus(); return; }
      Object.assign(pick, { liveFrom: $("#lv-from", el)?.value || "", stopAfter: secs });
    }
    let r = null;
    if (earlyId && $("#url", el).value.trim() === url) { // keep what's already downloaded
      try { r = await api("early", { action: "commit", id: earlyId, ...pick, start, queueId: start ? "" : qid }); } catch (e) { r = { error: String(e.message || e) }; }
      if (r.error && r.field === "checksum") return badSum(cap1(r.error) + ".");
      if (r.error) { toast(t("fi.keepFail", { error: r.error }), 5000); r = null; } else committed = true;
    }
    const sameUrl = $("#url", el).value.trim() === url;
    if (!r) r = await api("add", { url: $("#url", el).value.trim(), ...pick, start, queueId: start ? "" : qid, headers, ...(stream ? stream.pick() : {}), size: !hls && !ml && sameUrl && info.size > 0 ? info.size : 0,
      ...(sameUrl ? { method: req.method, body: req.body } : {}), private: req.private });
    if (r.error) {
      if (r.field === "checksum") return badSum(cap1(r.error) + ".");
      throw new Error(r.error);
    }
    added = true;
    el.close();
    if (start && (S.settings?.showProgressDialog ?? true)) openProgressWhenReady(r.id);
    if (!start) toast(qid === PLAN_Q && S.plan && S.plan.on ? th("n.addedFree", { window: planWindowText(S.plan) })
      : th("n.addedQueue", { queue: qName(qid) ? nm(qName(qid)) : t("n.theQueue") }), 4000);
  }
}
// folderSaved says where a shared folder's files go: all of them into a folder of its name, one on its own.
const folderSaved = (f, all) => all ? ht("fi.folderSaved", { name: nm(f.name) }) : ht("fi.folderOne");
// folderFacts are a shared folder's lines in the Add dialog: how many files, where they go, the service.
function folderFacts(info, st) {
  const f = info.folder;
  return `
      <div>${te("fact.type")}</div><div class="wrap">${ht(f.total >= 0 ? "fi.folderSize" : "fi.folder", { n: f.files.length, size: fmtSize(f.total) })}</div>
      <div>${te("fact.savedAs")}</div><div class="wrap" id="fi-fsaved">${folderSaved(f, f.files.length > 1)}</div>
      <div>${te("fact.resume")}</div><div>${ht("fi.resumeYes", { n: st.conns })}</div>
      <div>${te("fact.from")}</div><div>${ht("fi.fromShared", { service: info.service })}</div>`;
}
// ---- Torrents in the Add dialog
const byPath = (a, b) => a.path.localeCompare(b.path, undefined, { numeric: true, sensitivity: "base" });
// tfRow is one file in a torrent's file list: a tick to download it, how far it got (Properties),
// its size and its priority (Skip, Low, Normal, High). prio maps a file to -1, 0 or 1.
const PRIO = [["skip", "prio.skip"], ["-1", "prio.low"], ["0", "prio.normal"], ["1", "prio.high"]];
const prioValue = (on, p) => on ? String(p || 0) : "skip";
function tfRow(f, on, prio, pct) {
  const at = f.path.lastIndexOf("/"), name = f.path.slice(at + 1), v = prioValue(on, prio);
  return `<div class="tf-row" title="${esc(f.path)}"><label class="tf-pick"><input type="checkbox" data-i="${f.i}"${on ? " checked" : ""}><span class="tf-name">${at >= 0 ? `<span class="tf-dir">${esc(f.path.slice(0, at + 1))}</span>` : ""}${esc(name)}</span></label>${pct ? `<span class="tf-pct" data-p="${f.i}"></span>` : ""}<span class="tf-size">${fmtSize(f.size)}</span><select class="inp tf-prio" aria-label="${te("tor.prioAria", { name })}">${PRIO.map(([x, id]) => `<option value="${x}"${x === v ? " selected" : ""}>${te(id)}</option>`).join("")}</select></div>`;
}
// wireFiles keeps a file list's ticks and priorities in step (unticked is Skip; a priority for a
// skipped file ticks it) and calls changed after each change.
function wireFiles(box, files, checked, prio, changed) {
  const rows = $$(".tf-row", box).map(r => ({ c: $("input", r), s: $("select", r) }));
  for (const { c, s } of rows) {
    const i = +c.dataset.i;
    c.onchange = () => { c.checked ? checked.add(i) : checked.delete(i); s.value = prioValue(c.checked, prio.get(i)); changed(); };
    s.onchange = () => { if (s.value === "skip") checked.delete(i); else { checked.add(i); prio.set(i, +s.value); } c.checked = s.value !== "skip"; changed(); };
  }
  $("#tf-all", box).onchange = e => {
    const on = e.target.checked;
    files.forEach(f => on ? checked.add(f.i) : checked.delete(f.i));
    for (const { c, s } of rows) { c.checked = on; s.value = prioValue(on, prio.get(+c.dataset.i)); }
    changed();
  };
}
// orderBoxes are a torrent's picking options: in order (to watch while it downloads), and each
// file's first and last pieces first.
const orderBoxes = idp => `<div class="t-order"><label class="chk"><input type="checkbox" id="${idp}-seq"> ${te("tor.seq")}</label><label class="chk"><input type="checkbox" id="${idp}-ends"> ${te("tor.ends")}</label></div><div class="hint t-order-h">${te("tor.orderHint")}</div>`;
// torrentDialog fills the Add dialog for a torrent: where to save, which files, and (for a
// magnet link) the file list arriving from peers. It returns the dialog's go/close handlers.
function torrentDialog(el, url, info, preset = {}) {
  const st = S.settings || {};
  let files = info.files || null, count = info.fileCount || 0, metaTimer = 0, closed = false;
  const checked = new Set(), prio = new Map();
  const magnet = /^magnet:/i.test(url);
  const setFiles = (fs, n) => { files = [...fs].sort(byPath); count = n; checked.clear(); fs.forEach(f => { if (!f.skip) checked.add(f.i); }); };
  let catTouched = false;
  if (files) setFiles(files, count);
  $(".t", el).textContent = t("tor.title");
  const body = $("#fi", el) || $(".dlg-b", el).firstElementChild;
  const wrap = h(`<div class="tdlg">
    ${info.exists ? `<div class="info-box"><div>${ht("tor.exists", { name: nm(info.exists) })}</div></div>` : ""}
    <div class="t-head">${catIcon(info.category || "General")}<div class="t-title"><b id="t-name"></b><span class="hint" id="t-sub" style="margin:0"></span></div></div>
    ${field(te("f.category"), `<select class="inp" id="cat">${catOptions(info.category || "General")}</select>`)}
    ${field(te("f.saveTo"), pickControl("dir", "browse"))}
    ${field(te("f.laterInto"), `<select class="inp" id="qsel">${S.queues.map(q => `<option value="${q.id}">${esc(q.name)}</option>`).join("")}</select>`)}
    <div class="tfiles" id="t-files"></div>
    ${orderBoxes("t")}
    <div class="kv" style="margin-top:12px" id="t-facts"></div>
  </div>`);
  body.replaceWith(wrap);
  $("#dir", el).value = preset.dir || info.dir || st.downloadDir || "";
  const setCat = c => { $("#cat", el).value = c; $("#dir", el).value = st.categoryFolders && c !== "General" ? joinPath(st.downloadDir, c) : st.downloadDir; };
  let dirTouched = !!preset.dir;
  $("#cat", el).onchange = () => { catTouched = true; if (!dirTouched) setCat($("#cat", el).value); };
  $("#dir", el).addEventListener("input", () => { dirTouched = true; });
  $("#browse", el).onclick = () => pickFolder($("#dir", el));
  const tq = preset.queueId || queueTarget(); if (tq && S.queues.some(q => q.id === tq)) $("#qsel", el).value = tq;
  const selSize = () => (files || []).filter(f => checked.has(f.i)).reduce((a, f) => a + f.size, 0);
  const draw = () => {
    $("#t-name", el).textContent = info.name || t("tor.default");
    const n = (files || []).length;
    $("#t-sub", el).textContent = files ? t(info.private ? "tor.subPrivate" : "tor.sub", { size: fmtSize(info.size), n }) : t("tor.sizeUnknown");
    const box = $("#t-files", el);
    if (!files) {
      box.innerHTML = `<div class="t-wait"><span class="spin"></span><div><b>${te("tor.getting")}</b><div class="hint" style="margin:2px 0 0" id="t-peers">${te("tor.looking")}</div></div></div>`;
    } else if (n > 1) {
      const rows = files.slice(0, 3000).map(f => tfRow(f, checked.has(f.i), prio.get(f.i))).join("");
      box.innerHTML = `<div class="tf-head"><label class="chk"><input type="checkbox" id="tf-all"> ${te("tor.files")}</label><span class="hint" id="tf-sel" style="margin:0"></span></div><div class="tf-list">${rows}</div>${n > 3000 ? `<div class="hint">${te("tor.firstN", { n: 3000 })}</div>` : ""}`;
      const sync = () => {
        const all = $("#tf-all", box); all.checked = checked.size === n; all.indeterminate = checked.size > 0 && checked.size < n;
        $("#tf-sel", box).textContent = t("tor.chosen", { checked: checked.size, n, size: fmtSize(selSize()) });
        $("#start", el).disabled = $("#later", el).disabled = !checked.size || !!info.exists;
      };
      wireFiles(box, files, checked, prio, sync);
      sync();
    } else box.innerHTML = "";
    $("#t-facts", el).innerHTML = `
      <div>${te("fact.type")}</div><div>${ht("tor.type", { magnet: magnet ? "yes" : "no", priv: info.private ? "yes" : "no" })}</div>
      ${info.pieces ? `<div>${te("fact.pieces")}</div><div class="wrap">${ht("tor.pieces", { n: info.pieces, size: fmtSize(info.pieceLen) })}</div>` : ""}
      <div>${te("fact.findingPeers")}</div><div class="wrap">${esc([info.trackers ? t("tor.trackers", { n: info.trackers }) : "", info.private ? "" : st.torrent?.noDHT ? "" : t("tor.dht"), info.webSeeds ? t("tor.webSeeds", { n: info.webSeeds }) : ""].filter(Boolean).join(" · ") || t("tor.inbound"))}</div>
      ${info.comment ? `<div>${te("fact.comment")}</div><div class="wrap">${esc(info.comment)}</div>` : ""}`;
  };
  draw();
  $("#later", el).disabled = $("#start", el).disabled = !!info.exists;
  if (!info.exists) $("#start", el).focus();
  if (!files && magnet) {
    const poll = async () => {
      if (closed) return;
      const r = await api("torrent/meta", { magnet: url }).catch(() => null);
      if (closed || !el.isConnected) return;
      if (r && r.files) {
        const keep = { trackers: Math.max(info.trackers || 0, r.trackers || 0), webSeeds: Math.max(info.webSeeds || 0, r.webSeeds || 0) };
        Object.assign(info, r, keep); setFiles(r.files, r.fileCount);
        if (!catTouched && r.category) { $(".t-head", el).firstElementChild.outerHTML = catIcon(r.category); if (!dirTouched) setCat(r.category); else $("#cat", el).value = r.category; }
        draw(); return;
      }
      const p = $("#t-peers", el);
      if (p && r) p.textContent = r.peers ? t(r.metaPct ? "tor.connectedPct" : "tor.connected", { n: r.peers, pct: fmtPct(r.metaPct || 0, 0) }) : t("tor.lookingStart");
      metaTimer = setTimeout(poll, 1500);
    };
    poll();
  }
  return {
    go: async start => {
      const skip = files && count ? Array.from({ length: count }, (_, i) => !checked.has(i)) : [];
      const pr = files && count ? Array.from({ length: count }, (_, i) => prio.get(i) || 0) : [];
      const qid = $("#qsel", el).value;
      const r = await api("torrent/add", { magnet: url, skip, prio: pr, seq: $("#t-seq", el).checked, ends: $("#t-ends", el).checked, dir: $("#dir", el).value.trim(), start, queueId: start ? "" : qid, category: catTouched ? $("#cat", el).value : "" });
      if (r.error) throw new Error(cap1(r.error));
      closed = true; clearTimeout(metaTimer);
      el.close();
      if (start && (S.settings?.showProgressDialog ?? true)) openProgressWhenReady(r.id);
      if (!start) toast(th("n.addedTo", { queue: qName(qid) ? nm(qName(qid)) : t("n.theQueue") }));
    },
    close: () => {
      if (!closed && magnet) api("torrent/meta", { magnet: url, stop: true }).catch(() => {});
      closed = true; clearTimeout(metaTimer);
    },
  };
}

async function pickFolder(input) {
  // a modal card, so it sits above whichever window asked and nothing else can be pressed meanwhile
  const ov = h(`<dialog class="pick-card" role="alert" tabindex="-1"><span class="spin" aria-hidden="true"></span>${te("pick.wait")}</dialog>`);
  ov.addEventListener("cancel", e => e.preventDefault());
  ov.addEventListener("keydown", e => e.stopPropagation()); // (the page's own Esc would close the window underneath)
  // focus goes to the card itself, or a key pressed meanwhile (Esc) would still reach the button that asked
  document.body.appendChild(ov); ov.showModal(); ov.focus();
  try {
    const r = await api("pickdir", { start: input.value });
    if (r.dir) { input.value = r.dir; input.dispatchEvent(new Event("input", { bubbles: true })); }
    else if (r.error) fieldError(input, cap1(r.error));
  } catch (e) { fieldError(input, t("pick.fail")); }
  finally { ov.close(); ov.remove(); input.focus(); }
}
function joinPath(a, b) { const sep = a.includes("\\") ? "\\" : "/"; return a.replace(/[\\/]+$/, "") + sep + b; }
async function pasteAdd() { const text = (await clipText()).trim(); addUrlDialog(LINK_RE.test(text) ? text : ""); }

// ---- Progress dialog
// A new download shows up with the next live update (~0.4 s); wait for it.
function openProgressWhenReady(id, tries = 0) {
  if (S.byId.has(id)) return progressDialog(id);
  if (tries < 25) setTimeout(() => openProgressWhenReady(id, tries + 1), 120);
}
// wireTabs makes a row of tab buttons work like a tab list: one tab is in the Tab order, the arrow keys move
// between them, and show(name) is told which one is chosen.
function wireTabs(list, show) {
  const tabs = $$("[role=tab]", list);
  const pick = (b, focus) => {
    tabs.forEach(x => { const on = x === b; x.classList.toggle("active", on); x.setAttribute("aria-selected", String(on)); x.tabIndex = on ? 0 : -1; });
    show(b.dataset.t); if (focus) b.focus();
  };
  tabs.forEach(b => b.onclick = () => pick(b));
  list.addEventListener("keydown", e => {
    const i = tabs.indexOf(document.activeElement), n = tabs.length;
    const j = i < 0 ? -1 : { ArrowRight: (i + 1) % n, ArrowLeft: (i + n - 1) % n, Home: 0, End: n - 1 }[e.key] ?? -1;
    if (j >= 0) { e.preventDefault(); pick(tabs[j], true); }
  });
  pick(tabs.find(x => x.classList.contains("active")) || tabs[0]);
}
// The facts every progress window shows, in this order for a plain download, a stream and a torrent. The last
// row is the one thing that only fits some kinds (resumable, or how much a torrent has shared).
const progressFacts = extra => `<div class="kv p-facts">
        <div>${te("fact.link")}</div><div class="p-url"></div>
        <div>${te("fact.status")}</div><div class="p-st wrap"></div>
        <div>${te("fact.size")}</div><div class="p-size"></div>
        <div>${te("fact.downloaded")}</div><div class="p-done"></div>
        <div>${te("fact.rate")}</div><div class="p-speed"></div>
        <div>${te("fact.timeLeft")}</div><div class="p-eta"></div>
        <div>${te("fact.conns")}</div><div class="p-conn wrap"></div>
        <div>${esc(extra)}</div><div class="p-extra wrap"></div>
      </div>
      ${progressBar(0, "", { cls: "bigbar", fill: "p-bar", label: t("pf.progress") })}`;
const progressLimit = `<div class="limit-row">
        <label class="chk"><input type="checkbox" class="p-lim"> ${te("pf.limit")}</label>
        <input class="inp num p-limkb" type="number" min="1" step="50" aria-label="${te("pf.limitAria")}"><span class="unit">${te("unit.kilobyte-per-second")}</span>
      </div>`;
const progressHide = `<button class="btn p-hide" aria-expanded="true">${te("pf.hide")}</button>`;
// wireProgress hooks up what both kinds of progress window share: Hide details and the speed limit.
function wireProgress(el, id, d) {
  $(".p-hide", el).onclick = e => {
    const det = $(".p-det", el), hide = !det.hidden;
    det.hidden = $("canvas", el).hidden = hide;
    e.currentTarget.textContent = t(hide ? "pf.show" : "pf.hide"); e.currentTarget.setAttribute("aria-expanded", String(!hide));
  };
  const lim = $(".p-lim", el), kb = $(".p-limkb", el);
  lim.checked = d.limitKB > 0; kb.value = d.limitKB > 0 ? d.limitKB : 500; kb.disabled = !lim.checked;
  let timer;
  const apply = () => { clearTimeout(timer); timer = setTimeout(() => api("limit", { id, kb: lim.checked ? Math.max(1, +kb.value || 1) : 0 }).catch(e => el.fail(e)), 300); };
  // ticking the box limits the download to the number beside it (and unticking lifts the limit); the
  // key that ticked it stays on the box, so Space never does anything else
  lim.onchange = () => { kb.disabled = !lim.checked; apply(); };
  kb.oninput = apply;
}
// setFacts fills the rows of progressFacts that are the same whatever is downloading.
function setFacts(el, d, f) {
  const u = $(".p-url", el); if (u.textContent !== f.url) { u.textContent = f.url; u.title = f.url; }
  const st = $(".p-st", el); st.textContent = f.status; st.style.color = f.statusColor || ""; st.title = d.error || "";
  $(".p-size", el).textContent = f.size; $(".p-done", el).textContent = f.done; $(".p-speed", el).textContent = f.speed; $(".p-eta", el).textContent = f.eta;
  $(".p-conn", el).textContent = f.conn; $(".p-extra", el).innerHTML = f.extra;
  const bar = $(".p-bar", el); setProgress(bar, f.pct, f.text || progressText(d)); bar.classList.toggle("ok", !!f.ok);
}
function progressDialog(id) {
  if (S.progress.has(id)) { raiseDialog(S.progress.get(id)); return; }
  const d = S.byId.get(id); if (!d) return;
  if (d.torrent) return torrentProgress(id);
  if (d.status === "completed") return completeDialog(id);
  // modeless: several of these can be open beside the list
  const el = dialog({
    title: th("pd.titleName", { name: nm(dname(d)) }), width: 580, modal: false, focus: "dialog",
    body: `${progressFacts(t("fact.resumable"))}
      <canvas class="segmap" height="16" aria-hidden="true"></canvas>
      <div class="p-det"><div class="conns"><table><thead><tr><th style="width:42px">${te("pd.connN")}</th><th>${te("pd.connDone")}</th><th>${te("pd.connInfo")}</th></tr></thead><tbody class="p-conns"></tbody></table></div>
        <div class="p-srcs" hidden><div class="section-title"><span>${te("pd.servers")}</span><span class="p-pchk"></span></div><div class="conns"><table><thead><tr><th>${te("pd.server")}</th><th style="width:96px">${te("pd.connections")}</th><th style="width:110px">${te("pd.received")}</th></tr></thead><tbody class="p-srcrows"></tbody></table></div></div></div>
      ${progressLimit}`,
    left: progressHide,
    buttons: [
      { label: t("a.pause"), id: "pp", fn: () => { const x = S.byId.get(id); act(["downloading", "connecting", "queued"].includes(x?.status) ? "pause" : "start", [id]); } },
      { label: t("btn.cancel"), cancel: true, fn: el => { const x = S.byId.get(id); if (x && ["downloading", "connecting"].includes(x.status)) act("pause", [id]); el.close(); } },
    ],
    onClose: () => S.progress.delete(id),
  });
  wireProgress(el, id, d);
  S.progress.set(id, el);
  updateProgress(id);
}
// srcNote says why a server of the progress window isn't used any more, or how many damaged pieces it sent.
// (on a narrow window it goes on a line of its own)
const srcNote = x => x.dead ? ` <span class="why"><span class="sep">· </span>${x.damaged ? te("pd.dropDamaged") : esc(x.dead)}</span>`
  : x.bad ? ` <span class="why"><span class="sep">· </span>${ht("pd.srcBad", { n: x.bad })}</span>` : "";
function updateProgress(id) {
  const el = S.progress.get(id); const d = S.byId.get(id);
  if (!el) return;
  if (!d) { el.close(); return; }
  if (el._update) return el._update(d);
  const p = pct(d);
  put($(".t", el), d.size > 0 ? th("pd.title", { pct: fmtPct(Math.floor(p), 0), name: nm(dname(d)) }) : th("pd.titleName", { name: nm(dname(d)) }));
  const stId = { downloading: "pd.st.downloading", connecting: "pd.st.connecting", paused: "pd.st.paused", queued: "pd.st.queued", completed: "pd.st.completed" }[d.status];
  const stWord = d.status === "error" ? (d.link ? d.error : t("pd.st.error", { error: d.error || "" })) : stId ? t(stId) : d.status;
  const hls = isHLS(d), live = (d.segs || []).filter(s => s[3] === "receiving" || s[3] === "connecting").length;
  setFacts(el, d, {
    url: d.url,
    status: d.stage && d.status === "downloading" ? t("pd.stage", { stage: d.stage, pct: fmtPct(d.stagePct || 0, 0) }) : stWord, statusColor: d.status === "error" ? "var(--err)" : "",
    size: hls ? (d.status === "completed" ? fmtSize(d.size) : d.est > 0 ? t("pd.estimate", { size: fmtSize(d.est) }) : t("pd.workingOut")) : d.size > 0 ? fmtSize(d.size) : t("size.unknown"),
    done: hls ? t("pd.doneHls", { done: fmtSize(d.done), pct: fmtPct(p, 2), piece: (d.partsDone || 0) + (d.aDone || 0), total: d.parts ? d.parts + (d.aParts || 0) : "?" }) : d.size > 0 ? t("pd.doneOf", { done: fmtSize(d.done), pct: fmtPct(p, 2) }) : fmtSize(d.done),
    speed: fmtSpeed(d.speed) || "—",
    eta: d.status === "downloading" && !d.stage ? fmtTime(etaOf(d)) || "—" : "—",
    conn: d.status !== "downloading" || d.stage ? "—" : hls ? (d.workers > 0 ? t("pd.fetching", { n: d.workers }) : "—") : !d.resumable ? t("pd.oneOnly") : live ? t(d.conns ? "pd.activeMax" : "pd.active", { n: live, max: d.conns }) : "—",
    extra: hls || d.resumable ? ht("yn.yes") : d.size < 0 ? "—" : ht("yn.no"),
    pct: p,
  });
  const pp = $("#pp", el); pp.textContent = t(["downloading", "connecting", "queued"].includes(d.status) ? "a.pause" : "a.resume");
  // segment map
  const cv = $("canvas", el);
  const W = cv.clientWidth * devicePixelRatio | 0, H = 16 * devicePixelRatio | 0;
  if (W > 0) {
    if (cv.width !== W) { cv.width = W; cv.height = H; }
    const g = cv.getContext("2d"); const css = getComputedStyle(document.documentElement);
    g.fillStyle = css.getPropertyValue("--barBg"); g.fillRect(0, 0, W, H);
    if (d.size > 0 && d.segs) {
      for (const [s, e, pos, st] of d.segs) {
        const x0 = s / d.size * W, x1 = Math.max(x0 + 1, (Math.min(pos, e + 1)) / d.size * W);
        g.fillStyle = st === "done" ? css.getPropertyValue("--segDone") : css.getPropertyValue("--seg");
        g.fillRect(x0, 0, x1 - x0, H);
        if (st !== "done" && pos <= e) { g.fillStyle = st === "receiving" ? "#ffb020" : css.getPropertyValue("--segIdle"); g.fillRect(x1, 0, Math.max(2, devicePixelRatio * 2), H); }
      }
    } else if (d.status === "completed" || (d.size > 0 && d.done >= d.size)) { g.fillStyle = css.getPropertyValue("--segDone"); g.fillRect(0, 0, W, H); }
    else if (d.size > 0) { g.fillStyle = css.getPropertyValue("--seg"); g.fillRect(0, 0, d.done / d.size * W, H); }
    else if (hls && d.parts > 0) { g.fillStyle = css.getPropertyValue("--seg"); g.fillRect(0, 0, pct(d) / 100 * W, H); }
  }
  const segText = s => ["receiving", "connecting", "retry", "failed", "done", "idle"].includes(s) ? t("seg." + s) : s;
  const rows = hls ? `<tr><td colspan="3" style="color:var(--muted)">${d.workers > 0 ? esc(t("pd.hlsConns", { n: d.workers })) : te("pd.noConns")}</td></tr>` : (d.segs || []).filter(s => s[3] !== "done" && s[3] !== "idle").map((s, i) =>
    `<tr><td>${i + 1}</td><td>${fmtSize(s[2] - s[0])} / ${fmtSize(s[1] - s[0] + 1)}</td><td>${esc(segText(s[3]))}</td></tr>`).join("") ||
    `<tr><td colspan="3" style="color:var(--muted)">${te(d.status === "downloading" && !d.resumable ? "pd.noRanges" : "pd.noConns")}</td></tr>`;
  const tb = $(".p-conns", el); if (tb._h !== rows) { tb.innerHTML = rows; tb._h = rows; }
  const srcs = d.sources || [], checks = d.pieces > 0 && d.status !== "completed" && (d.piecesOk > 0 || d.piecesBad > 0); // (a metalink's piece checks, once under way)
  $(".p-srcs", el).hidden = srcs.length < (checks ? 1 : 2);
  put($(".p-pchk", el), checks ? th(d.piecesBad ? "pd.pieceCheckBad" : "pd.pieceCheck", { ok: d.piecesOk || 0, n: d.pieces, bad: d.piecesBad || 0 }) : "");
  const srows = srcs.map(x => `<tr${x.dead ? ` class="dim"` : ""}><td title="${esc(x.url)}">${esc(x.host)}${x.mirror ? "" : ` <span class="tag">${te("pd.main")}</span>`}${srcNote(x)}</td><td>${x.dead ? "—" : x.conns}</td><td>${fmtSize(x.bytes)}</td></tr>`).join("");
  const sb = $(".p-srcrows", el); if (sb._h !== srows) { sb.innerHTML = srows; sb._h = srows; }
}

// ---- Torrent progress: piece map, peers, trackers and files
const b64bytes = s => { try { return Uint8Array.from(atob(s || ""), c => c.charCodeAt(0)); } catch { return new Uint8Array(0); } };
// drawPieceMap paints a torrent's parts: green = here, blue shade = partly here, hatched = not downloading.
function drawPieceMap(cv, map, done) {
  const W = cv.clientWidth * devicePixelRatio | 0, H = 16 * devicePixelRatio | 0;
  if (W <= 0) return;
  if (cv.width !== W) { cv.width = W; cv.height = H; }
  const g = cv.getContext("2d"), css = getComputedStyle(document.documentElement);
  g.globalAlpha = 1; g.fillStyle = css.getPropertyValue("--barBg"); g.fillRect(0, 0, W, H);
  if (done && !map.length) { g.fillStyle = css.getPropertyValue("--segDone"); g.fillRect(0, 0, W, H); return; }
  const n = map.length; if (!n) return;
  const hatch = document.createElement("canvas"); hatch.width = hatch.height = 6 * devicePixelRatio | 0;
  const hg = hatch.getContext("2d"); hg.strokeStyle = css.getPropertyValue("--segIdle"); hg.lineWidth = devicePixelRatio;
  hg.beginPath(); hg.moveTo(0, hatch.height); hg.lineTo(hatch.width, 0); hg.stroke();
  const pat = g.createPattern(hatch, "repeat");
  for (let b = 0; b < n; b++) {
    const v = map[b], x0 = Math.floor(b / n * W), x1 = Math.max(x0 + 1, Math.floor((b + 1) / n * W));
    if (v === 0) continue;
    if (v === 255) { g.globalAlpha = 1; g.fillStyle = pat; }
    else if (v >= 100) { g.globalAlpha = 1; g.fillStyle = css.getPropertyValue("--segDone"); }
    else { g.globalAlpha = .3 + .7 * v / 100; g.fillStyle = css.getPropertyValue("--seg"); }
    g.fillRect(x0, 0, x1 - x0, H);
  }
  g.globalAlpha = 1;
}
// seedLimitText says when a torrent stops sharing: its own ratio and time when it has them, else Options'.
function seedLimitText(tor) {
  const tc = tor && tor.seed === "custom" ? tor : S.settings?.torrent || {};
  const ratio = tor && tor.stopRatio || tc.seedRatio; // (sharing again counts from where it began: the torrent says where it stops)
  return tc.seedRatio > 0 && tc.seedMins > 0 ? t("tp.stopsAt", { ratio: +ratio.toFixed(2), time: fmtIn(tc.seedMins * 60) }) : "";
}
// trackerRow is one line of a torrent's tracker list: what it answered, in words, and what it said besides
// (a warning, the reason it turned Zipline away) and when it is asked again.
const TRACKER_STATES = ["waiting", "asking", "working", "standby", "nodns", "noanswer", "noconn", "refused", "says", "error"];
function trackerRow(x, tiers) {
  const status = TRACKER_STATES.includes(x.state) ? t("tp.ts." + x.state, { text: x.arg || "" }) : x.status || "—";
  const notes = [x.warning && t("tp.trkWarn", { text: x.warning }), x.detail && t("tp.trkReply", { text: x.detail }), x.retryIn && t("tp.trkRetry", { span: fmtIn(x.retryIn) })].filter(Boolean);
  return `<tr><td title="${esc(x.announce || x.url)}">${esc(x.url)}${tiers ? ` <span class="t-note">· ${te("tp.trkTier", { n: (x.tier || 0) + 1 })}</span>` : ""}</td>`
    + `<td class="wrap" title="${esc([status, ...notes].join("\n"))}"><div>${esc(status)}</div>${notes.map(n => `<div class="t-note">${esc(n)}</div>`).join("")}</td>`
    + `<td class="num">${x.peers || (x.seeders + x.leechers) || "—"}</td></tr>`;
}
function torrentProgress(id) {
  const d = S.byId.get(id);
  const el = dialog({
    title: th("pd.titleName", { name: nm(dname(d)) }), width: 580, modal: false, focus: "dialog",
    body: `${progressFacts(t("fact.shared"))}
      <canvas class="segmap" height="16" title="${te("tp.mapTip")}" aria-hidden="true"></canvas>
      <div class="p-det">
        <div class="tabs t-tabs" role="tablist" aria-label="${te("tp.details")}">
          <button class="tab active" data-t="peers" role="tab" id="tt-peers">${te("tp.peers")}</button><button class="tab" data-t="trackers" role="tab" id="tt-trackers">${te("tp.trackers")}</button><button class="tab" data-t="files" role="tab" id="tt-files">${te("tp.files")}</button>
        </div>
        <div class="conns t-pane" data-t="peers" role="tabpanel" aria-labelledby="tt-peers"><table><thead><tr><th>${te("tp.peer")}</th><th class="t-client" style="width:130px">${te("tp.client")}</th><th class="num" style="width:58px">${te("tp.has")}</th><th class="num" style="width:84px">${te("tp.down")}</th><th class="num" style="width:84px">${te("tp.up")}</th></tr></thead><tbody class="t-peers"></tbody></table></div>
        <div class="conns t-pane" data-t="trackers" role="tabpanel" aria-labelledby="tt-trackers" hidden><table><thead><tr><th>${te("tp.tracker")}</th><th style="width:44%">${te("tp.status")}</th><th class="num" style="width:64px">${te("tp.peers")}</th></tr></thead><tbody class="t-trackers"></tbody></table></div>
        <div class="t-foot" data-t="trackers" hidden><button type="button" class="btn sm" id="tt-edit">${te("tp.editTrackers")}</button></div>
        <div class="conns t-pane" data-t="files" role="tabpanel" aria-labelledby="tt-files" hidden><table><thead><tr><th>${te("tp.file")}</th><th class="num" style="width:84px">${te("tp.size")}</th><th style="width:110px">${te("tp.done")}</th></tr></thead><tbody class="t-files"></tbody></table></div>
      </div>
      ${progressLimit}`,
    left: progressHide,
    buttons: [
      { label: t("a.pause"), id: "pp", fn: () => {
        const x = S.byId.get(id); if (!x) return;
        if (x.torrent?.seeding) api("action", { action: "stopSharing", ids: [id] });
        else act(["downloading", "connecting", "queued"].includes(x.status) ? "pause" : "start", [id]);
      } },
      { label: t("btn.cancel"), id: "pc", cancel: true, fn: el => { const x = S.byId.get(id); if (x && ["downloading", "connecting"].includes(x.status)) act("pause", [id]); el.close(); } },
    ],
    onClose: () => { S.progress.delete(id); clearTimeout(timer); },
  });
  let det = null, timer = 0;
  const poll = async () => {
    const r = await api("torrent/detail", { id }).catch(() => null);
    if (!el.isConnected) return;
    if (r && !r.error) { det = r; const x = S.byId.get(id); if (x) el._update(x); }
    timer = setTimeout(poll, 1000);
  };
  wireTabs($(".t-tabs", el), t => $$(".t-pane, .t-foot", el).forEach(p => p.hidden = p.dataset.t !== t));
  $("#tt-edit", el).onclick = () => propsDialog(S.byId.get(id), "trackers");
  wireProgress(el, id, d);
  const setRows = (sel, html) => { const tb = $(sel, el); if (tb._h !== html) { tb.innerHTML = html; tb._h = html; } };
  const empty = (n, text) => `<tr><td colspan="${n}" class="t-empty">${esc(text)}</td></tr>`;
  el._update = d => {
    const tt = Object.assign({}, d.torrent, det || {}, d.torrent); // live numbers from the list, details from the poll
    const active = ["downloading", "connecting"].includes(d.status), seeding = !!tt.seeding, p = pct(d);
    put($(".t", el), d.size > 0 && !seeding ? th("pd.title", { pct: fmtPct(Math.floor(p), 0), name: nm(dname(d)) }) : th("pd.titleName", { name: nm(dname(d)) }));
    const lim = seedLimitText(tt);
    setFacts(el, d, {
      url: tt.magnet || t("tp.torrentFile"),
      status: seeding ? t(lim ? "tp.sharingLimit" : "tp.sharing", { limit: lim })
        : d.status === "completed" ? t("st.complete") : tt.needMeta && active ? (tt.metaPct ? t("tp.gettingPct", { pct: fmtPct(tt.metaPct, 0) }) : t("tor.getting"))
        : d.status === "downloading" && tt.checking ? t("st.checkingFiles", { pct: fmtPct(tt.checkPct || 0, 0) })
        : d.status === "downloading" ? t(tt.peers ? "pd.st.downloading" : "tp.looking")
        : d.status === "error" ? t("pd.st.error", { error: d.error || "" }) : { connecting: t("pd.st.connecting"), paused: t("pd.st.paused"), queued: t("pd.st.queued") }[d.status] || d.status,
      statusColor: d.status === "error" ? "var(--err)" : seeding ? "var(--ok)" : "",
      size: tt.needMeta ? t("tp.notKnown") : det && det.files && det.files.length > 1 ? t("tp.sizeFiles", { size: fmtSize(d.size), chosen: det.files.filter(f => !f.skip).length, n: det.files.length }) : fmtSize(d.size),
      done: tt.needMeta ? "—" : t("pd.doneOf", { done: fmtSize(Math.min(d.done, d.size)), pct: fmtPct(p, 2) }),
      speed: t(tt.held ? "tp.ratesHeld" : "tp.rates", { down: fmtSpeed(d.speed, true), up: fmtSpeed(tt.up, true) }),
      eta: d.status === "downloading" && !tt.needMeta ? fmtTime(etaOf(d)) || "—" : "—",
      conn: active || seeding ? t("tp.conn", { n: tt.peers || 0, seeds: tt.seeds || 0, known: tt.known || 0 }) : "—",
      extra: esc(t("tp.uploaded", { size: fmtSize(tt.uploaded || 0), ratio: fmtRatio(tt.ratio || 0) })),
      pct: seeding ? 100 : p, ok: seeding || d.status === "completed", text: seeding ? t("st.completeSharing") : progressText(d),
    });
    const pp = $("#pp", el);
    pp.textContent = t(seeding ? "a.stopSharing" : ["downloading", "connecting", "queued"].includes(d.status) ? "a.pause" : d.status === "completed" ? "a.shareAgain" : "a.resume");
    $("#pc", el).textContent = t(seeding || d.status === "completed" ? "btn.close" : "btn.cancel");
    $(".limit-row", el).hidden = seeding || d.status === "completed";
    drawPieceMap($("canvas", el), b64bytes(tt.map), d.status === "completed");
    // peers
    const peers = tt.peerList || [];
    setRows(".t-peers", peers.length ? peers.map(x => { const pm = peerMarks(x.marks); return `<tr title="${[esc(x.flags || ""), ...pm.words].filter(Boolean).join(" · ")}"><td>${pm.icons}${esc(x.addr)}</td><td class="t-client">${esc(x.client || "—")}</td><td class="num">${fmtPct(Math.round(x.have * 100), 0)}</td><td class="num">${fmtSpeed(x.down) || "—"}</td><td class="num">${fmtSpeed(x.up) || "—"}</td></tr>`; }).join("")
      : empty(5, t(active || seeding ? (det ? "tp.noPeers" : "tp.loading") : "tp.notRunning")));
    $(".t-tabs [data-t=peers]", el).textContent = peers.length ? t("tp.peersCount", { n: peers.length }) : t("tp.peers");
    // trackers (and the DHT)
    const tr = tt.trackers || [];
    const dht = tt.private ? "" : S.settings?.torrent?.noDHT ? "" : `<tr><td>${te("tp.dht")} <span class="t-note">${te("tp.dhtNote")}</span></td><td>${active || seeding ? te("tp.on") : "—"}</td><td class="num">—</td></tr>`;
    const tiers = new Set(tr.map(x => x.tier)).size > 1;
    setRows(".t-trackers", tr.map(x => trackerRow(x, tiers)).join("") + dht
      || empty(3, t("tp.noTrackers")));
    // files
    const fs = [...(det && det.files || [])].sort(byPath);
    setRows(".t-files", fs.length ? fs.map(f => `<tr${f.skip ? ` class="dim"` : ""}><td title="${esc(f.path)}">${esc(f.path)}</td><td class="num">${fmtSize(f.size)}</td><td>${f.skip ? te("tp.skipped") : `<div class="t-fbar" aria-hidden="true"><i style="width:${((f.done || 0) * 100).toFixed(1)}%"></i></div>${fmtPct(Math.floor((f.done || 0) * 100), 0)}`}</td></tr>`).join("")
      : empty(3, t(tt.needMeta ? "tp.listArrives" : "tp.loading")));
  };
  S.progress.set(id, el);
  el._update(d);
  poll();
}

// peerMarks shows how a peer is connected: a lock when the connection is encrypted (dimmer when
// only the handshake is), a small tag for uTP; their words (and the fast extension's) go in the row's tooltip.
const PEER_LOCK = `<svg width="11" height="11" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" aria-hidden="true"><rect x="2.8" y="7" width="10.4" height="7.6" rx="1.6" fill="currentColor"/><path d="M5.3 7V5.1a2.7 2.7 0 0 1 5.4 0V7"/></svg>`;
function peerMarks(marks) {
  const has = new Set((marks || "").split(" ")), words = [];
  let icons = "";
  for (const [m, word, tip] of [["enc", te("tp.mEnc"), te("tp.mEncTip")], ["obf", te("tp.mObf"), te("tp.mObfTip")], ["utp", te("tp.mUtp"), te("tp.mUtpTip")], ["fast", te("tp.mFast"), te("tp.mFastTip")]]) {
    if (!has.has(m)) continue;
    words.push(word);
    if (m === "enc" || m === "obf") icons += `<span class="t-mark${m === "obf" ? " dim" : ""}" title="${tip}">${PEER_LOCK}</span>`;
    else if (m === "utp") icons += `<span class="t-mark tag" title="${tip}">${word}</span>`;
  }
  return { icons, words };
}

// ---- Refresh download link
async function refreshLink(d, from) {
  if (!d) return;
  if (!d.page) return propsDialog(d, "url");
  const r = await api("refreshlink", { id: d.id }).catch(() => ({}));
  // asked from inside a window (Properties): answer there, a toast would sit under it
  const say = (m, ms) => from && !from.closed ? from.note(m) : toast(m, ms);
  if (r.result === "auto") say(th("n.freshLink", { host: nm(hostName(d.page)) }), 4000);
  else if (r.result === "page") say(th("n.pageOpen"), 7000);
  else propsDialog(d, "url");
}
const hostName = u => { try { return new URL(u).hostname.replace(/^www\./, ""); } catch { return u; } };
const ALGO = { md5: "MD5", sha1: "SHA-1", sha256: "SHA-256", sha512: "SHA-512", quickxor: "QuickXorHash" };
const SUM_LEN = { 32: "md5", 40: "sha1", 64: "sha256", 128: "sha512" };
// sumGuess says what a pasted checksum looks like ("" when it's empty, null when it isn't one).
function sumGuess(v) {
  v = v.trim();
  if (!v) return "";
  if (/\n/.test(v)) return "list";
  let m, hex = "";
  if ((m = v.match(/^(?:md5|sha-?1|sha-?256|sha-?512)\s*[:= ]\s*([0-9a-f]+)$/i))) hex = m[1];
  else if ((m = v.match(/^(?:md5|sha-?1|sha-?256|sha-?512)\s*\(.+\)\s*=\s*([0-9a-f]+)$/i))) hex = m[1];
  else if ((m = v.match(/^([0-9a-f]+)(?:\s+\*?.+)?$/i))) hex = m[1];
  const algo = SUM_LEN[hex.length];
  if (algo) return ALGO[algo];
  return /^(?:(?:md5|sha-?1|sha-?256|sha-?512)\s*[:=]\s*)?[A-Za-z0-9+/]{22,}={0,2}$/i.test(v) ? "base64" : null;
}
// sumGuessName says in words what sumGuess found.
const sumGuessName = g => g === "list" ? t("fi.sumList") : g === "base64" ? t("fi.sumBase64") : g;
function sumSourceText(c) {
  return !c.from || c.from === "you" ? t("sum.you") : c.from === "server" ? t("sum.server") : c.from === "metalink" ? t("sum.metalink") : c.from === "calculated" ? "" : t("sum.from", { name: c.from });
}

// ---- Complete dialog
function completeDialog(id) {
  const d = S.byId.get(id); if (!d) return;
  // modeless: it pops up by itself when a download finishes, so it must not lock the page or grab Enter
  dialog({
    title: t("cd.title"), width: 520, key: "done-" + id, modal: false, focus: "dialog",
    body: `<div style="display:flex;gap:12px;align-items:flex-start">
      <div style="transform:scale(2.2);transform-origin:top left;width:36px;height:36px;flex:none">${catIcon(d.category)}</div>
      <div class="kv" style="flex:1;min-width:0">
        <div>${te("fact.file")}</div><div title="${esc(d.name)}"><b>${esc(d.name)}</b></div>
        <div>${te("fact.size")}</div><div>${fmtSize(d.size)}</div>
        <div>${te("fact.savedTo")}</div><div title="${esc(d.dir)}">${esc(d.dir)}</div>
        <div>${te("fact.link")}</div><div title="${esc(d.url)}">${esc(d.url)}</div>
      </div></div>`,
    left: `<label class="chk" style="margin:0"><input type="checkbox" class="nomore"> ${te("cd.noMore")}</label>`,
    buttons: [
      { label: t("a.open"), primary: true, busy: t("btn.opening"), fn: el => { api("action", { action: "open", ids: [id] }); return finish(el); } },
      { label: t("a.openFolder"), busy: t("btn.opening"), fn: el => { api("action", { action: "folder", ids: [id] }); return finish(el); } },
      { label: t("btn.close"), cancel: true, fn: el => finish(el) },
    ],
  });
  async function finish(el) {
    if ($(".nomore", el).checked) { const st = await api("settings"); st.showCompleteDialog = false; S.settings = await api("settings", st); }
    el.close();
  }
}

// ---- Move to another folder
// moveDialog moves downloads (their files, part files and a torrent's whole folder) to a folder you choose. It
// stays open while the files move and says how far they got: between drives a big move takes a while.
function moveDialog(ids) {
  const ds = ids.map(id => S.byId.get(id)).filter(d => d && !d.move);
  if (!ds.length) return;
  const one = ds.length === 1 ? ds[0] : null;
  const kind = !one ? "many" : one.torrent ? "torrent" : one.status === "completed" ? "done" : "active";
  const el = dialog({
    title: one ? th("mv.title", { name: nm(dname(one)) }) : t("mv.titleMany", { n: ds.length }), width: 520, key: "move",
    body: field(te("f.moveTo"), pickControl("mv-dir", "mv-browse"), { hint: te("mv.hint", { kind }) }),
    buttons: [
      { label: t("mv.go"), primary: true, id: "mv-go", busy: t("mv.moving"), fn: async el => {
        const dir = $("#mv-dir", el).value.trim();
        if (!dir) { fieldError($("#mv-dir", el), t("mv.needDir")); $("#mv-dir", el).focus(); return; }
        const poll = setInterval(() => {
          const cur = ds.map(d => S.byId.get(d.id)?.move).filter(Boolean);
          if (cur.length) el.note(t("st.moving", { pct: fmtPct(cur.reduce((a, m) => a + m.pct, 0) / cur.length, 0) }));
        }, 400);
        let r;
        try { r = await api("move", { ids: ds.map(d => d.id), dir }); } finally { clearInterval(poll); }
        if (r.error) throw new Error((r.moved ? t("mv.some", { n: r.moved }) + " " : "") + cap1(r.error) + ".");
        toast(t("mv.done", { n: r.moved, dir }), 4000);
        el.close();
      } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
  });
  $("#mv-dir", el).value = ds[0].dir;
  $("#mv-browse", el).onclick = () => pickFolder($("#mv-dir", el));
}
// folderField is Properties' "Saved to": where the files are, and a button to move them. drawFolder keeps it
// up to date (and shows a move in progress).
const folderField = () => field(te("fact.savedTo"), `<div class="pick"><input class="inp" id="pdir" readonly spellcheck="false"><button type="button" class="btn browse" id="pmove" title="${te("mv.tip")}">${I.folderOpen}<span>${te("mv.btn")}</span></button></div>`);
function drawFolder(el, d) {
  const dir = $("#pdir", el), text = d.move ? t("st.moving", { pct: fmtPct(d.move.pct, 0) }) : d.dir;
  if (dir.value !== text) { dir.value = text; dir.title = d.dir; }
  $("#pmove", el).disabled = !!d.move;
}

// ---- Properties
function propsDialog(d, focus) {
  if (!d) { toast(t("n.selectOne")); return; }
  if (d.torrent) return torrentProps(d, focus);
  const id = d.id, hls = isHLS(d);
  const el = dialog({
    title: th("pr.title", { name: nm(dname(d)) }), width: 580,
    body: `<div class="info-box" id="plink" hidden></div>
      ${field(te("f.fileName"), `<input class="inp" id="pn" spellcheck="false">`)}
      ${field(te("f.link"), `<input class="inp" id="pu" spellcheck="false">`, { hint: te("pr.linkHint") })}
      ${hls ? "" : field(te("f.mirrors"), `<textarea class="inp" id="pm" rows="2" spellcheck="false" placeholder="${te("pr.mirrorsPh")}"></textarea>`, { top: true })}
      ${field(te("f.desc"), `<input class="inp" id="pd">`)}
      ${folderField()}
      ${field(te("f.speedLimit"), `<div class="inline"><input class="inp num" id="plim" type="number" min="0" step="50"><span class="unit">${te("pr.limitUnit")}</span></div>`)}
      ${field(te("f.virusScan"), `<select class="inp" id="pscan">
        <option value="">${te("pr.scanDefault", { state: S.settings?.scan?.on ? "on" : "off" })}</option><option value="on">${te("pr.scanOnFile")}</option><option value="off">${te("pr.scanOffFile")}</option></select>`)}
      ${hls ? "" : field(te("f.checksum"), `<input class="inp" id="psum" spellcheck="false" placeholder="${te("pr.sumPh")}">`, { hint: `<div class="sum-state" id="psumst"></div>` })}
      <div class="kv" style="margin-top:12px">
        <div>${te("fact.category")}</div><div>${esc(catName(d.category))}</div>
        <div>${te("fact.size")}</div><div>${d.size >= 0 ? esc(t("pr.sizeBytes", { size: fmtSize(d.size), bytes: d.size })) : te("size.unknown")}</div>
        <div>${te("fact.status")}</div><div class="wrap" id="pst"></div>
        <div>${te("fact.added")}</div><div>${fmtDate(d.added)}</div>
        <div>${te("fact.finished")}</div><div>${fmtDate(d.finished) || "—"}</div>
        <div>${te("fact.conns")}</div><div>${d.conns}</div>
        <div>${te("fact.queue")}</div><div>${esc(qName(d.queue)) || "—"}</div>
        ${d.page ? `<div>${te("fact.fromPage")}</div><div title="${esc(d.page)}">${esc(d.page)}</div>` : ""}
        <div id="pexpk" hidden>${te("fact.linkExpires")}</div><div id="pexp" hidden></div>
        ${d.scanState ? `<div>${te("fact.virusScan")}</div><div class="scan ${d.scanState}" title="${esc(d.scanNote || "")}">${esc(d.scanNote || (SCAN_STATES.includes(d.scanState) ? t("scan." + d.scanState) : ""))}</div>` : ""}
        ${d.syncNote ? `<div>${te("fact.syncCheck")}</div><div>${esc(d.syncNote)}${fmtDate(d.checked) ? " · " + fmtDate(d.checked) : ""}</div>` : ""}
      </div>`,
    buttons: [
      { label: t("btn.save"), primary: true, busy: t("btn.saving"), fn: async el => {
        const x = S.byId.get(id) || d, sum = $("#psum", el), want = sum ? sum.value.trim() : "";
        const bad = (ctl, msg) => { fieldError(ctl, msg); ctl.focus(); };
        if (!$("#pu", el).value.trim()) return bad($("#pu", el), t("pr.needLink"));
        if (sum && sumGuess(want) === null) return bad(sum, t("fi.sumBad"));
        // the checksum first: it is the one the server is most likely to refuse, so nothing else is changed then
        if (sum && (want !== sumWant(x) || x.status === "completed" && want && !(x.check && x.check.state))) {
          const r = await api("checksum", { id, want });
          if (r.error) return bad(sum, cap1(r.error) + ".");
        }
        const body = { id, name: $("#pn", el).value, url: $("#pu", el).value, desc: $("#pd", el).value, scan: $("#pscan", el).value || "default" };
        if ($("#pm", el)) body.mirrors = $("#pm", el).value.split(/\s+/).filter(Boolean);
        const r = await api("props", body);
        if (r.error) return /renam/i.test(r.error) ? bad($("#pn", el), cap1(r.error) + ".") : Promise.reject(new Error(r.error));
        const kb = Math.max(0, +$("#plim", el).value || 0);
        if (kb !== (x.limitKB || 0)) await api("limit", { id, kb });
        el.close();
      } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
  });
  const sumWant = x => x.check && x.check.want ? x.check.want : "";
  const setSumState = h => { const n = $("#psumst", el); if (n && n._h !== h) { n.innerHTML = h; n._h = h; wireSum(); } };
  function wireSum() {
    const calc = $("#psumcalc", el); if (calc) calc.onclick = () => api("checksum", { id, calc: true });
    const cp = $("#psumcopy", el); if (cp) cp.onclick = () => copyText(el, cp.dataset.v, t("pr.sumCopied"));
    const go = $("#plinkgo", el); if (go) go.onclick = () => refreshLink(S.byId.get(id) || d, el);
  }
  $("#pmove", el).onclick = () => moveDialog([id]);
  $("#pn", el).value = d.name; $("#pu", el).value = d.url; $("#pd", el).value = d.desc || ""; $("#plim", el).value = d.limitKB || 0; $("#pscan", el).value = d.scan || "";
  if ($("#pm", el)) $("#pm", el).value = (d.mirrors || []).join("\n");
  if ($("#psum", el)) $("#psum", el).value = sumWant(d);
  const draw = () => {
    const x = S.byId.get(id) || d;
    drawFolder(el, x);
    $("#pst", el).textContent = statusText(x) + (x.error && !x.link ? " — " + x.error : "");
    $("#pu-h", el).hidden = !!x.link || x.status === "completed";
    const lb = $("#plink", el);
    lb.hidden = !x.link;
    if (x.link) {
      const host = x.page ? nm(hostName(x.page)) : "";
      const h = x.link === "refreshing" ? `<div>${ht("pr.linkRefreshing", { host })}</div>`
        : x.link === "waiting" ? `<div>${ht("pr.linkWaiting", { host })}</div>`
        : `<div>${ht(x.page ? "pr.linkExpiredPage" : "pr.linkExpired", { host })}</div>${x.page ? `<button class="btn sm" id="plinkgo">${te("pr.getFresh")}</button>` : ""}`;
      if (lb._h !== h) { lb.innerHTML = h; lb._h = h; wireSum(); }
    }
    const exp = x.expires && x.status !== "completed" ? new Date(x.expires * 1000) : null;
    $("#pexpk", el).hidden = $("#pexp", el).hidden = !exp;
    if (exp) $("#pexp", el).textContent = exp < Date.now() ? t("pr.expired", { span: fmtIn((Date.now() - exp) / 1000) }) : t("pr.expires", { when: cap1(whenText(exp)), span: fmtIn((exp - Date.now()) / 1000) });
    if (!$("#psumst", el)) return;
    const c = x.check, done = x.status === "completed";
    let h;
    if (done && x.stage) h = `<span class="spin"></span>${te("pr.checking", { pct: fmtPct(x.stagePct || 0, 0) })}`;
    else if (c && c.want && c.state === "ok") h = ht("sum.matches", { algo: ALGO[c.algo], has: sumSourceText(c) ? "y" : "n", src: sumSourceText(c) });
    else if (c && c.want && c.state === "bad") h = `${ht("sum.mismatch", { algo: ALGO[c.algo], src: sumSourceText(c), redownload: t("a.redownload") })}<div class="sum-got">${ht("sum.gotAlgo", { algo: ALGO[c.algo], got: c.got || "" })}</div>`;
    else if (c && c.want) h = ht("sum.pending", { algo: ALGO[c.algo], src: sumSourceText(c), done: done ? "y" : "n", pieces: x.pieces || 0 });
    else if (c && c.got) h = `<div class="sum-got">${ht("sum.sha256", { got: c.got })} <button class="btn sm" id="psumcopy" data-v="${esc(c.got)}">${te("btn.copy")}</button></div>`;
    else if (done) h = ht("sum.pasteOr", { calc: raw(`<button class="btn sm" id="psumcalc">${te("sum.calc")}</button>`) });
    else h = te(S.settings?.checksums === false ? "sum.pasteOnly" : "sum.ifSite");
    setSumState(h);
  };
  el._draw = draw; draw();
  const sumIn = $("#psum", el);
  if (sumIn) sumIn.oninput = () => { const g = sumGuess(sumIn.value); sumIn.classList.toggle("invalid", g === null); };
  if (focus === "sum" && sumIn) { sumIn.focus(); }
  if (focus === "url") { $("#pu", el).focus(); $("#pu", el).select(); }
}

// The trackers as text: a line each, a blank line between tiers (the trackers of a tier are tried in order,
// the next tier only when they all fail).
const tiersText = tiers => (tiers || []).map(x => x.join("\n")).join("\n\n");
const parseTiers = text => text.split(/\r?\n/).reduce((a, line) => {
  line = line.trim();
  if (!line) { if (a.at(-1).length) a.push([]); } else a.at(-1).push(line);
  return a;
}, [[]]).filter(x => x.length);

// torrentProps is Properties for a torrent: its magnet link, which files to download, sharing, trackers.
function torrentProps(d, focus) {
  const id = d.id;
  let files = null, checked = new Set(), prio = new Map(), touched = false, timer = 0, trk = null, trkTouched = false;
  const el = dialog({
    title: th("pr.title", { name: nm(dname(d)) }), width: 600,
    body: `<div class="t-head">${catIcon(d.category)}<div class="t-title"><b><bdi>${esc(dname(d))}</bdi></b><span class="hint" id="tp-sub" style="margin:0"></span></div><button class="btn sm" id="tp-check" title="${te("tp.checkTip")}">${te("ctx.checkFiles")}</button></div>
      ${field(te("f.magnet"), `<div class="pick"><input class="inp" id="tp-mag" readonly spellcheck="false"><button class="btn browse" id="tp-copy">${te("btn.copy")}</button></div>`)}
      ${field(te("f.desc"), `<input class="inp" id="pd">`)}
      ${folderField()}
      ${field(te("tp.limitDown"), `<div class="inline"><input class="inp num" id="plim" type="number" min="0" step="50"><span class="unit">${te("pr.limitUnit")}</span></div>`)}
      ${field(te("tp.limitUp"), `<div class="inline"><input class="inp num" id="ptup" type="number" min="0" step="10"><span class="unit">${te("pr.limitUnit")}</span></div>`)}
      ${field(te("f.virusScan"), `<select class="inp" id="pscan">
        <option value="">${te("pr.scanDefault", { state: S.settings?.scan?.on ? "on" : "off" })}</option><option value="on">${te("pr.scanOnFiles")}</option><option value="off">${te("pr.scanOffFiles")}</option></select>`)}
      <details class="fold" id="tp-more"><summary>${te("tp.moreTitle")}</summary><div class="fold-body">
        ${field(te("tp.shareLbl"), `<select class="inp" id="ptseed"><option value=""></option><option value="custom">${te("tp.shareCustom")}</option><option value="off">${te("tp.shareOff")}</option></select>`, { hint: te("tp.shareHint") })}
        ${field(te("o.tStop"), `<div class="inline"><span class="unit">${te("o.tRatio")}</span><input class="inp num" id="ptratio" type="number" min="0.1" step="0.1" aria-label="${te("o.tRatioAria")}"><span class="unit">${te("o.tOrAfter")}</span><select class="inp fit" id="ptmins" aria-label="${te("o.tTimeAria")}"></select></div>`, { group: true, id: "ptstop", attrs: `id="ptstop-row"` })}
        ${field(te("tp.trackers"), `<textarea class="inp" id="ptrk" rows="5" spellcheck="false" placeholder="${te("tp.trkPh")}"></textarea>`, { top: true, hint: te("tp.trkHint") })}
      </div></details>
      <div class="tfiles" id="tp-files"></div>
      ${orderBoxes("tp")}
      <div class="kv" style="margin-top:12px">
        <div>${te("fact.status")}</div><div class="wrap" id="pst"></div>
        <div>${te("fact.shared")}</div><div id="tp-up"></div>
        <div>${te("fact.infoHash")}</div><div class="tp-hash" title="${esc(d.torrent.hash)}">${esc(d.torrent.hash)}</div>
        <div>${te("fact.added")}</div><div>${fmtDate(d.added)}</div>
        <div>${te("fact.finished")}</div><div id="tp-fin"></div>
        <div>${te("fact.queue")}</div><div>${esc(qName(d.queue)) || "—"}</div>
      </div>`,
    buttons: [
      { label: t("btn.save"), primary: true, id: "tp-save", busy: t("btn.saving"), fn: async el => {
        const x = S.byId.get(id) || d, tor = x.torrent;
        const kb = Math.max(0, +$("#plim", el).value || 0), upKB = Math.max(0, Math.round(+$("#ptup", el).value || 0));
        const mode = $("#ptseed", el).value, ratio = +$("#ptratio", el).value;
        if (mode === "custom" && !(ratio > 0)) { $("#tp-more", el).open = true; fieldError($("#ptratio", el), t("o.errRatio")); $("#ptratio", el).focus(); return; }
        const share = mode === "" ? { seed: "", seedRatio: 0, seedMins: 0 } : mode === "off" ? { seed: "custom", seedRatio: 0, seedMins: 0 } : { seed: "custom", seedRatio: Math.round(ratio * 100) / 100, seedMins: +$("#ptmins", el).value };
        // the trackers first: the server may well refuse an address, and then nothing else is changed
        if (trkTouched) {
          const r = await api("torrent/trackers", { id, tiers: parseTiers($("#ptrk", el).value) });
          if (r.error) { $("#tp-more", el).open = true; fieldError($("#ptrk", el), cap1(r.error) + "."); $("#ptrk", el).focus(); return; }
        }
        if (upKB !== (tor.upKB || 0) || share.seed !== (tor.seed || "") || share.seedRatio !== (tor.seedRatio || 0) || share.seedMins !== (tor.seedMins || 0)) {
          const r = await api("torrent/limits", { id, upKB, ...share });
          if (r.error) throw new Error(cap1(r.error) + ".");
        }
        if (kb !== (x.limitKB || 0)) await api("limit", { id, kb });
        const r = await api("props", { id, desc: $("#pd", el).value, scan: $("#pscan", el).value || "default" });
        if (r.error) throw new Error(r.error);
        const pick = { id };
        if (files && touched) {
          pick.skip = Array.from({ length: files.count }, (_, i) => !checked.has(i));
          pick.prio = Array.from({ length: files.count }, (_, i) => prio.get(i) || 0);
        }
        for (const k of ["seq", "ends"]) if ($(`#tp-${k}`, el).checked !== !!x.torrent[k]) pick[k] = $(`#tp-${k}`, el).checked;
        if (Object.keys(pick).length > 1) {
          const r2 = await api("torrent/files", pick);
          if (r2.error) throw new Error(cap1(r2.error));
        }
        el.close();
      } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
    onClose: () => clearTimeout(timer),
  });
  $("#pd", el).value = d.desc || ""; $("#plim", el).value = d.limitKB || 0; $("#pscan", el).value = d.scan || "";
  $("#tp-mag", el).value = d.torrent.magnet || "";
  $("#pmove", el).onclick = () => moveDialog([id]);
  // sharing: this torrent's own ratio and time, or Options → Torrents' (the boxes start from those)
  {
    const tor = d.torrent, own = tor.seed === "custom", glob = S.settings?.torrent || {};
    $("#ptup", el).value = tor.upKB || 0;
    $("#ptseed", el).value = !own ? "" : tor.seedRatio > 0 && tor.seedMins > 0 ? "custom" : "off";
    $("#ptratio", el).value = own && tor.seedRatio > 0 ? tor.seedRatio : glob.seedRatio > 0 ? glob.seedRatio : 1;
    const mins = own && tor.seedMins > 0 ? tor.seedMins : glob.seedMins > 0 ? glob.seedMins : 120;
    $("#ptmins", el).innerHTML = [...new Set([30, 60, 120, 240, 480, 1440, 2880, mins])].sort((a, b) => a - b).map(n => `<option value="${n}">${fmtIn(n * 60)}</option>`).join("");
    $("#ptmins", el).value = mins;
    $("#ptseed", el).options[0].textContent = t("tp.shareDefault", { what: seedLimitText() || t("tp.shareNone") });
    const sync = () => { $("#ptstop-row", el).hidden = $("#ptseed", el).value !== "custom"; };
    $("#ptseed", el).onchange = sync; sync();
    $("#ptrk", el).oninput = () => { trkTouched = true; };
  }
  $("#tp-copy", el).onclick = () => copyText(el, $("#tp-mag", el).value, t("pr.magnetCopied"));
  $("#tp-seq", el).checked = !!d.torrent.seq; $("#tp-ends", el).checked = !!d.torrent.ends;
  $("#tp-check", el).onclick = () => { act("recheck", [id]); $("#tp-check", el).disabled = true; };
  const drawFiles = () => {
    const box = $("#tp-files", el);
    if (!files) {
      const x = S.byId.get(id) || d;
      const h = x.torrent.needMeta ? `<div class="t-wait"><span class="spin"></span><div><b>${te("tp.waitTitle")}</b><div class="hint" style="margin:2px 0 0">${te("tp.waitHint")}</div></div></div>` : "";
      if (box._h !== h) { box.innerHTML = h; box._h = h; }
      return;
    }
    const n = files.list.length;
    if (n < 2) { box.innerHTML = ""; return; }
    if (!box._built) {
      box.innerHTML = `<div class="tf-head"><label class="chk"><input type="checkbox" id="tf-all"> ${te("tp.filesToDownload")}</label><span class="hint" id="tf-sel" style="margin:0"></span></div><div class="tf-list">${files.list.slice(0, 3000).map(f => tfRow(f, checked.has(f.i), prio.get(f.i), true)).join("")}</div>${n > 3000 ? `<div class="hint">${te("tor.firstN", { n: 3000 })}</div>` : ""}<div class="hint" style="margin-bottom:0">${te("tp.untick")}</div>`;
      box._built = true;
      wireFiles(box, files.list, checked, prio, () => { touched = true; sync(); });
      if (focus === "files") box.scrollIntoView({ block: "nearest" });
    }
    for (const f of files.list.slice(0, 3000)) { const c = $(`[data-p="${f.i}"]`, box); if (c) c.textContent = f.skip ? "" : fmtPct(Math.floor((f.done || 0) * 100), 0); }
    sync();
  };
  const sync = () => {
    const box = $("#tp-files", el), n = files.list.length, all = $("#tf-all", box); if (!all) return;
    all.checked = checked.size === n; all.indeterminate = checked.size > 0 && checked.size < n;
    const size = files.list.filter(f => checked.has(f.i)).reduce((a, f) => a + f.size, 0);
    $("#tf-sel", box).textContent = t("tor.chosen", { checked: checked.size, n, size: fmtSize(size) });
    $("#tp-save", el).disabled = !checked.size;
  };
  const poll = async () => {
    const r = await api("torrent/detail", { id }).catch(() => null);
    if (!el.isConnected) return;
    if (r && !r.error && trk === null) { // the trackers as they are saved, unless you've started typing
      trk = tiersText(r.tiers);
      if (!trkTouched) $("#ptrk", el).value = trk;
    }
    if (r && r.files && r.files.length) {
      const fresh = !files;
      files = { list: [...r.files].sort(byPath), count: r.fileCount };
      if (fresh) { checked = new Set(r.files.filter(f => !f.skip).map(f => f.i)); prio = new Map(r.files.map(f => [f.i, f.prio || 0])); }
      else { const done = new Map(r.files.map(g => [g.i, g.done || 0])); files.list = files.list.map(f => Object.assign({}, f, { done: done.get(f.i) || 0 })); }
    }
    draw();
    timer = setTimeout(poll, 2000);
  };
  const draw = () => {
    const x = S.byId.get(id) || d, tr = x.torrent;
    drawFolder(el, x);
    $("#tp-sub", el).textContent = tr.needMeta ? t("tor.sizeUnknown") : t("tp.sub", { size: fmtSize(x.size), n: files && files.list.length > 1 ? files.list.length : 0, priv: tr.private ? "yes" : "no" });
    $("#pst", el).textContent = statusText(x) + (x.error ? " — " + x.error : "");
    $("#tp-up", el).textContent = t("tp.upState", { size: fmtSize(tr.uploaded || 0), ratio: fmtRatio(tr.ratio || 0), sharing: tr.seeding ? "yes" : "no" });
    $("#tp-fin", el).textContent = fmtDate(x.finished) || "—";
    $("#tp-check", el).disabled = !!tr.needMeta || !!tr.checking;
    drawFiles();
  };
  el._draw = draw;
  draw(); poll();
  if (focus === "files") $("#tp-files", el).scrollIntoView({ block: "nearest" });
  if (focus === "trackers") { $("#tp-more", el).open = true; $("#ptrk", el).focus(); }
}

// ---- Data plan helpers
// (PLAN_Q, hhmm, whenText and planWindowText are in i18n.js: the phone page says the free hours the same way)
const cap1 = s => s.charAt(0).toUpperCase() + s.slice(1);
function planEstimateText(p) {
  if (!p || !p.items) return t("plan.none");
  const finishes = p.finish && new Date(p.finish).getFullYear() > 2000;
  return t("plan.waiting", { items: p.items, left: p.remaining > 0 ? "yes" : "none", size: fmtSize(p.remaining),
    est: finishes ? (p.nights > 1 ? "periods" : "finish") : p.remaining > 0 ? "unknown" : "none", n: p.nights, when: finishes ? whenText(p.finish) : "" });
}
function planSummaryHTML(p) {
  const used = p.in ? ht("plan.usedSo", { size: fmtSize(p.usedFree) }) : (p.usedFree || p.usedPaid) ? ht("plan.usedLast", { free: fmtSize(p.usedFree), paid: fmtSize(p.usedPaid) }) : "";
  const when = p.in ? ht("plan.nowB", { time: hhmm(p.to) }) : ht("plan.nextB", { window: planWindowText(p) });
  return `<div>${when}</div><div>${esc(planEstimateText(p))}</div>${used ? `<div>${used}</div>` : ""}`;
}

// ---- Saved passwords. The app never sends one to the window, only whether one is saved (passSet). A password
// box made with passBox() says "(saved)" while it is empty and one is kept, and has a button that forgets it;
// passRead() is what to send back: typed text replaces the saved password, an untouched box keeps it, a forgotten one clears it.
const passBox = input => `<span class="pw">${input}<button type="button" class="pw-x" hidden title="${te("f.passForget")}" aria-label="${te("f.passForget")}"><svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><path d="M3 3l6 6M9 3 3 9"/></svg></button></span>`;
function passInit(inp, saved) {
  const x = $(".pw-x", inp.parentElement);
  inp._saved = !!saved; inp.value = "";
  const sync = () => { inp.placeholder = inp._saved ? t("f.passSaved") : ""; x.hidden = !(inp._saved && !inp.value); };
  inp.oninput = sync;
  x.onclick = () => { inp._saved = false; sync(); inp.focus(); };
  sync();
}
const passRead = inp => ({ pass: inp.value, passSet: !!inp._saved && !inp.value });
// vaultHint says how saved passwords are kept on this computer (the app's "vault" answer in the settings)
const vaultHint = v => v ? [ht("o.vault." + ({ "secret-service": "secret" }[v.how] || v.how)), v.locked ? ht("o.vault.locked") : "", v.plain ? ht("o.vault.plain") : ""].filter(Boolean).join(" ") : "";

// ---- Options
const OPT_SECTIONS = ["general", "conn", "plan", "sites", "proxy", "save", "cats", "scan", "sounds", "integration", "torrents", "phone"];
function keyOptions() {
  const alt = S.os === "darwin" ? "Option" : "Alt";
  return ["alt", "ctrlalt", "ctrlshift", "shiftalt", "off"].map(v => `<option value="${v}">${te("key." + v, { alt })}</option>`).join("");
}
// playChime plays an event sound on this computer, or in the window if the system has no player.
async function playChime(name, test) {
  try {
    if (test) { const r = await api(`sound?name=${name}`, {}); if (r.played) return; }
    await new Audio(`/api/sound?name=${encodeURIComponent(name)}`).play();
  } catch {}
}
async function optionsDialog(tab = "general", focus = "") {
  const st = await api("settings");
  const el = dialog({
    title: t("o.title"), width: 820, key: "options",
    onClose: () => { clearInterval(el._nets); clearInterval(el._phone); },
    body: `<div class="opts"><nav class="opts-nav">${OPT_SECTIONS.map(k => `<button class="otab" data-t="${k}">${te("opt." + k)}</button>`).join("")}</nav>
    <select class="inp opts-sel" aria-label="${te("o.sections")}">${OPT_SECTIONS.map(k => `<option value="${k}">${te("opt." + k)}</option>`).join("")}</select>
    <div class="opts-body">
    <div class="pane" data-p="general">
      <label class="chk"><input type="checkbox" id="o-prog"> ${te("o.prog")}</label>
      <label class="chk"><input type="checkbox" id="o-done"> ${te("o.done")}</label>
      <label class="chk"><input type="checkbox" id="o-early"> ${te("o.early")}</label>
      <div class="hint indent-chk">${te("o.earlyHint")}</div>
      <label class="chk" id="o-tray-row"><input type="checkbox" id="o-tray"> ${te("o.tray")}</label>
      <div class="hint indent-chk" id="o-tray-hint">${te("o.trayHint")}</div>
      ${field(te("f.theme"), `<select class="inp fit" id="o-theme"><option value="">${te("o.themeSystem")}</option><option value="light">${te("o.themeLight")}</option><option value="dark">${te("o.themeDark")}</option></select>`, { cls: "gap-t" })}
      <div class="section-title"><span>${te("o.power")}</span></div>
      <label class="chk"><input type="checkbox" id="o-awake"> ${te("o.awake")}</label>
      <div class="hint indent-chk">${te("o.awakeHint")}</div>
      <div class="section-title"><span>${te("o.startTitle")}</span></div>
      <div class="radio-list">
        <label class="radio"><input type="radio" name="o-start" value="resume"><div><b>${te("o.startResume")}</b><div class="hint">${te("o.startResumeHint")}</div></div></label>
        <label class="radio"><input type="radio" name="o-start" value="paused"><div><b>${te("o.startPaused")}</b><div class="hint">${te("o.startPausedHint")}</div></div></label>
      </div>
    </div>
    <div class="pane" data-p="conn">
      ${field(te("o.conns"), `<select class="inp fit" id="o-conns">${[1, 2, 4, 6, 8, 12, 16, 24, 32].map(n => `<option>${n}</option>`).join("")}</select>`, { hint: te("o.connsHint") })}
      ${field(te("o.maxact"), `<select class="inp fit" id="o-maxact">${[1, 2, 3, 4, 5, 6, 8, 10, 12, 15, 20, 30, 50].map(n => `<option>${n}</option>`).join("")}</select>`, { hint: ht("o.maxactHint", { link: raw(`<a href="#" id="o-goq">${te("o.queuesLink")}</a>`) }) })}
      ${field(te("a.speedLimiter"), `<div class="inline"><label class="chk lead"><input type="checkbox" id="o-lim"> ${te("o.limTotal")}</label><span class="group"><input class="inp num" id="o-limkb" type="number" min="1" aria-label="${te("o.limAria")}"><span class="unit">${te("unit.kilobyte-per-second")}</span></span></div>`, { group: true, id: "o-limg" })}
      ${field(te("o.cap"), `<div class="inline"><label class="chk lead"><input type="checkbox" id="o-cap"> ${te("o.capNoMore")}</label><span class="group"><input class="inp num" id="o-capmb" type="number" min="1" aria-label="${te("o.capAria")}"><span class="unit">${te("o.capUnit")}</span><select class="inp fit" id="o-caph" aria-label="${te("o.capHours")}">${[1, 2, 3, 4, 6, 8, 12, 24].map(n => `<option value="${n}">${te("o.hours", { n })}</option>`).join("")}</select></span></div>`, { group: true, id: "o-capg", hint: "", hintId: "o-capuse" })}
      <div class="hint">${te("o.limEach")}</div>
      <div class="section-title" id="o-tp"><span>${te("o.tp")}</span><button class="btn sm" id="o-tpadd">${I.plus}<span>${te("o.tpAdd")}</span></button></div>
      <div class="hint" style="margin-top:0">${te("o.tpHint")}</div>
      ${field(te("o.tpUse"), `<select class="inp fit" id="o-tpuse"></select>`, { labelId: "o-tpusel" })}
      <div class="site-list" id="o-tprofs"></div>
      <label class="chk"><input type="checkbox" id="o-tpsched"> ${te("o.tpSched")}</label>
      <div class="hint indent-chk">${te("o.tpSchedHint")}</div>
      <div class="sub" id="o-tpschedsub"><div class="rule-list" id="o-tprules"></div><button class="btn sm" id="o-tpruleadd">${I.plus}<span>${te("o.tpAddTime")}</span></button></div>
      <div class="section-title"><span>${te("o.sharing")}</span></div>
      <label class="chk"><input type="checkbox" id="o-polite"> ${te("o.polite")}</label>
      <div class="hint indent-chk">${te("o.politeHint")}</div>
      <div class="section-title"><span>${te("o.multiTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-multi"> ${te("o.multi")}</label>
      <div class="hint indent-chk">${te("o.multiHint")}</div>
      <div class="sub" id="o-multisub"><div class="net-list" id="o-nets"><div class="hint" style="margin:0">${te("o.netsLooking")}</div></div></div>
    </div>
    <div class="pane" data-p="plan">
      <label class="chk"><input type="checkbox" id="o-plan"> ${te("o.plan")}</label>
      <div class="hint indent-chk">${ht("o.planHint")}</div>
      <div class="sub" id="o-plansub">
        ${field(te("o.planFrom"), `<div class="inline"><input class="inp narrow" type="time" id="o-pfrom" aria-label="${te("o.planFromAria")}"><span class="unit">${te("o.planTo")}</span><input class="inp narrow" type="time" id="o-pto" aria-label="${te("o.planUntilAria")}"></div>`, { group: true, id: "o-ptime" })}
        ${field(te("o.planDays"), `<div class="days" id="o-pdays">${DAYS.map((d, i) => `<label><input type="checkbox" data-pd="${i}">${esc(d)}</label>`).join("")}</div>`, { group: true, id: "o-pdaysg" })}
        ${field(te("o.planMargin"), `<select class="inp fit" id="o-pmargin">${[0, 2, 5, 10, 15].map(n => `<option value="${n}">${te("o.minutes", { n })}</option>`).join("")}</select>`, { hint: te("o.planMarginHint") })}
        <label class="chk" style="margin-bottom:0"><input type="checkbox" id="o-upfree"> ${te("o.upFree")}</label>
        <div class="hint indent-chk" style="margin-bottom:0">${te("o.upFreeHint")}</div>
      </div>
      <div class="plan-sum" id="o-plansum" hidden></div>
      <div class="section-title"><span>${te("o.meterTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-meter"> ${te("o.meter")}</label>
      <div class="hint indent-chk" id="o-meterhint"></div>
      <div class="section-title" id="o-qsec"><span>${te("o.quotas")}</span><button class="btn sm" id="o-qadd">${I.plus}<span>${te("o.qAdd")}</span></button></div>
      <div class="hint" style="margin-top:0" id="o-qnet"></div>
      <div class="site-list" id="o-quotas"></div>
      ${field(te("o.qCount"), `<select class="inp fit" id="o-qcount"><option value="both">${te("o.qBoth")}</option><option value="down">${te("o.qDown")}</option><option value="up">${te("o.qUp")}</option></select>`, { hint: te("o.qCountHint") })}
      ${field(te("o.qWarn"), `<select class="inp fit" id="o-qwarn">${[50, 60, 70, 75, 80, 85, 90, 95].map(n => `<option value="${n}">${te("o.qWarnOpt", { pct: fmtPct(n, 0) })}</option>`).join("")}</select>`, { hint: te("o.qWarnHint") })}
    </div>
    <div class="pane" data-p="proxy">
      <div class="radio-list">
        <label class="radio"><input type="radio" name="o-pm" value="system"><div><b>${te("o.pmSystem")}</b><div class="hint">${te("o.pmSystemHint")}</div></div></label>
        <label class="radio"><input type="radio" name="o-pm" value="none"><div><b>${te("o.pmNone")}</b><div class="hint">${te("o.pmNoneHint")}</div></div></label>
        <label class="radio"><input type="radio" name="o-pm" value="manual"><div><b>${te("o.pmManual")}</b><div class="hint">${te("o.pmManualHint")}</div></div></label>
      </div>
      <div class="sub" id="o-pmanual">
        ${field(te("o.pAddr"), `<input class="inp" id="o-purl" spellcheck="false" placeholder="${te("o.pAddrPh")}">`)}
        ${field(te("f.user"), `<input class="inp" id="o-puser" autocomplete="off">`)}
        ${field(te("f.password"), passBox(`<input class="inp" id="o-ppass" type="password" autocomplete="off">`))}
        ${field(te("o.pBypass"), `<input class="inp" id="o-pbyp" spellcheck="false" placeholder="<local>; *.corp.example.com; 10.*">`)}
      </div>
      <div class="inline" style="margin-top:12px"><button class="btn sm" id="o-ptest">${te("o.pTest")}</button><span class="result" id="o-pres"></span></div>
      <div class="hint">${te("o.pFtp")}</div>
      <div class="hint" id="o-pvault"></div>
    </div>
    <div class="pane" data-p="scan">
      <div class="section-title" style="margin-top:0"><span>${te("o.sumsTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-sums"> ${te("o.sums")}</label>
      <div class="hint indent-chk">${ht("o.sumsHint")}</div>
      <div class="section-title"><span>${te("o.scanTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-scan"> ${te("o.scan")}</label>
      <div class="hint indent-chk">${te("o.scanHint")}</div>
      <div class="sub" style="margin-top:12px">
        ${field(te("o.scanner"), `<select class="inp fit" id="o-scanmode"><option value="auto">${te("o.scanAuto")}</option><option value="custom">${te("o.scanCustom")}</option></select>`, { hint: "", hintId: "o-scanauto" })}
        <div id="o-scancustom">
          ${field(te("o.scanProgram"), `<input class="inp" id="o-scanprog" spellcheck="false" placeholder="C:\\Program Files\\MyAV\\scan.exe">`)}
          ${field(te("o.scanArgs"), `<input class="inp" id="o-scanargs" spellcheck="false" placeholder="/scan %f">`, { hint: te("o.scanArgsHint") })}
        </div>
      </div>
    </div>
    <div class="pane" data-p="save">
      ${field(te("o.dir"), pickControl("o-dir", "o-browse"))}
      <label class="chk"><input type="checkbox" id="o-catf"> ${te("o.catf")}</label>
    </div>
    <div class="pane" data-p="cats">
      <div class="hint">${te("o.catsHint")}</div>
      <div id="o-cats"></div>
    </div>
    <div class="pane" data-p="integration">
      <label class="chk"><input type="checkbox" id="o-clip"> ${te("o.clip")}</label>
      <div class="hint">${te("o.clipHint")}</div>
      <div class="section-title"><span>${te("o.tkTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-takeover"> ${te("o.takeover")}</label>
      <div class="hint indent-chk">${te("o.takeoverHint")}</div>
      ${field(te("o.minSize"), `<div class="inline"><input class="inp num" id="o-minkb" type="number" min="0" step="64"><span class="unit">${te("unit.kilobyte")}</span></div>`)}
      ${field(te("o.skipTypes"), `<input class="inp" id="o-skiptypes" spellcheck="false" placeholder="html php json">`, { hint: ht("o.skipTypesHint") })}
      <div class="section-title"><span>${te("o.keysTitle")}</span></div>
      ${field(te("o.kZip"), `<select class="inp fit" id="o-kzip">${keyOptions()}</select>`)}
      ${field(te("o.kBrowser"), `<select class="inp fit" id="o-kbr">${keyOptions()}</select>`, { hint: te("o.kBrowserHint") })}
      <div class="section-title"><span>${te("o.skipTitle")}</span></div>
      <textarea class="inp" id="o-skip" rows="3" spellcheck="false" placeholder="bank.example.com&#10;intranet.example.com/files" aria-label="${te("o.skipAria")}" aria-describedby="o-skip-h"></textarea>
      <div class="hint" id="o-skip-h">${te("o.skipHint")}</div>
      <div class="section-title"><span>${te("o.vbTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-vbtn"> ${te("o.vbtn")}</label>
      <div class="hint indent-chk">${te("o.vbtnHint")}</div>
      <textarea class="inp" id="o-nobtn" rows="2" spellcheck="false" placeholder="video.example.com" aria-label="${te("o.noBtnAria")}" aria-describedby="o-nobtn-h"></textarea>
      <div class="hint" id="o-nobtn-h">${te("o.noBtnHint")}</div>
      <div class="section-title"><span>${te("o.expiredTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-refresh"> ${te("o.refresh")}</label>
      <div class="hint indent-chk">${te("o.refreshHint")}</div>
      <div class="section-title"><span>${te("o.extTitle")}</span></div>
      <div id="o-link"></div>
      <div class="hint" style="margin-top:0">${te("o.extBrowsers")}</div>
      <ol style="margin:6px 0 0 18px;padding:0;line-height:1.7">
        <li>${ht("o.extStep1")}</li>
        <li>${ht("o.extStep2")}</li>
        <li>${ht("o.extStep3")}</li>
      </ol>
      <div class="hint">${ht("o.extHint")}</div>
    </div>
    <div class="pane" data-p="torrents">
      <div class="section-title"><span>${te("o.afterTitle")}</span></div>
      <div class="radio-list">
        <label class="radio"><input type="radio" name="o-ts" value="share"><div><b>${te("o.tsShare")}</b><div class="hint">${te("o.tsShareHint")}</div></div></label>
        <label class="radio"><input type="radio" name="o-ts" value="off"><div><b>${te("o.tsOff")}</b><div class="hint">${te("o.tsOffHint")}</div></div></label>
      </div>
      <div class="sub" id="o-tsub">
        ${field(te("o.tStop"), `<div class="inline"><span class="unit">${te("o.tRatio")}</span><input class="inp num" id="o-tratio" type="number" min="0.1" step="0.1" aria-label="${te("o.tRatioAria")}"><span class="unit">${te("o.tOrAfter")}</span><select class="inp fit" id="o-tmins" aria-label="${te("o.tTimeAria")}"></select></div>`, { group: true, id: "o-tstop", hint: te("o.tStopHint") })}
        ${field(te("o.tAtMost"), `<div class="inline"><select class="inp fit" id="o-tmaxseeds" aria-label="${te("o.tAtMostAria")}">${[1, 2, 3, 5, 8, 10, 20, 50].map(n => `<option value="${n}">${n}</option>`).join("")}</select><span class="unit">${te("o.tAtMostUnit")}</span></div>`, { group: true, id: "o-tmost", hint: te("o.tAtMostHint") })}
      </div>
      <label class="chk"><input type="checkbox" id="o-tcheck"> ${te("o.tCheckDone")}</label>
      <div class="hint indent-chk">${te("o.tCheckDoneHint")}</div>
      <div class="section-title"><span>${te("o.tSpeedTitle")}</span></div>
      <div class="info-box" id="o-tproxy" role="note" hidden><div></div></div>
      ${field(te("o.tUp"), `<div class="inline"><input class="inp num" id="o-tup" type="number" min="0" step="10"><span class="unit">${te("pr.limitUnit")}</span></div>`)}
      ${field(te("o.tPeers"), `<select class="inp fit" id="o-tpeers">${[20, 40, 60, 80, 100].map(n => `<option value="${n}">${n}</option>`).join("")}</select>`)}
      ${field(te("o.tMeta"), `<select class="inp fit" id="o-tmeta">${[5, 10, 15, 30, 60, 120].map(n => `<option value="${n}">${esc(fmtIn(n * 60))}</option>`).join("")}<option value="-1">${te("o.tNoLimit")}</option></select>`, { hint: te("o.tMetaHint") })}
      ${field(te("o.tPort"), `<input class="inp num" id="o-tport" type="number" min="1024" max="65535" placeholder="${te("o.tPortAuto")}">`, { hint: te("o.tPortHint") })}
      <label class="chk"><input type="checkbox" id="o-tmap" aria-describedby="o-tmaph o-tmapst"> ${te("o.tMap")}</label>
      <div class="hint indent-chk" id="o-tmaph">${te("o.tMapHint")}</div>
      <div class="hint indent-chk ph-st" id="o-tmapst" role="status" hidden></div>
      <label class="chk"><input type="checkbox" id="o-tdht"> ${te("o.tDht")}</label>
      <div class="hint indent-chk">${te("o.tDhtHint")}</div>
      ${field(te("o.tEnc"), `<select class="inp fit" id="o-tenc"><option value="">${te("o.tEncPrefer")}</option><option value="require">${te("o.tEncRequire")}</option><option value="plain">${te("o.tEncPlain")}</option></select>`, { hint: te("o.tEncHint") })}
      <div class="section-title"><span>${te("o.tOpenTitle")}</span></div>
      <label class="chk"><input type="checkbox" id="o-tbrowser"> ${te("o.tBrowser")}</label>
      <div class="hint indent-chk">${te("o.tBrowserHint")}</div>
      <label class="chk" id="o-tassocrow" hidden><input type="checkbox" id="o-tassoc"> ${te("o.tAssoc")}</label>
      <div class="hint indent-chk" id="o-tassochint"></div>
    </div>
    <div class="pane" data-p="phone">
      <label class="chk"><input type="checkbox" id="o-phone"> ${te("o.phone")}</label>
      <div class="hint indent-chk">${te("o.phoneHint")}</div>
      <div class="sub" id="o-phonesub">
        ${field(te("o.phPort"), `<input class="inp num" id="o-phport" type="number" min="1024" max="65535">`, { hint: "", hintId: "o-phfw" })}
        <div class="hint indent ph-st" id="o-phst" hidden></div>
        <div id="o-phnets"></div>
      </div>
      <div class="info-box ph-http" role="note"><div>${te("o.phHttp")}</div></div>
      <div class="section-title"><span>${te("o.phones")}</span><button class="btn sm" id="o-phadd">${I.plus}<span>${te("o.phAdd")}</span></button></div>
      <div class="pair-box" id="o-pairbox" hidden>
        <div class="qr" id="o-qr"></div>
        <div class="pair-text">
          <b>${te("o.phScan")}</b>
          <div class="hint" style="margin:4px 0 10px">${ht("o.phOpen", { url: raw(`<code id="o-phurl"></code>`) })}</div>
          <div class="pair-code" id="o-pcode"></div>
          ${field(te("o.phAddr"), `<select class="inp" id="o-paddr"></select>`, { attrs: `id="o-paddrrow" hidden style="margin:10px 0 0"` })}
          <div class="pair-wait" id="o-pwait"><span class="spin"></span>${te("o.phWaiting")}</div>
        </div>
      </div>
      <div class="dev-list" id="o-devs"></div>
      <div class="hint">${te("o.phKeyHint")}</div>
    </div>
    <div class="pane" data-p="sites">
      <div class="hint" style="margin-top:0">${ht("o.sitesHint")}</div>
      <div class="site-list" id="o-sites"></div>
      <button class="btn sm" id="o-siteadd">${I.plus}<span>${te("o.siteAdd")}</span></button>
      <div class="hint" id="o-sitehint"></div>
    </div>
    <div class="pane" data-p="sounds">
      <label class="chk"><input type="checkbox" id="o-snd"> ${te("o.snd")}</label>
      <div class="sub" id="o-sndopts">
        ${[["done", "o.sndDone"], ["failed", "o.sndFailed"], ["queue", "o.sndQueue"]].map(([k, id]) => `<div class="snd-row"><label class="chk"><input type="checkbox" id="o-snd-${k}"> ${te(id)}</label><button class="btn sm" data-play="${k}" title="${te("o.sndPlayTip")}">${I.play}<span>${te("o.sndPlay")}</span></button></div>`).join("")}
      </div>
      <div class="hint">${te("o.sndHint")}</div>
    </div>
    </div></div>`,
    // OK saves and closes, Cancel throws the edits away, Apply saves and stays (the same buttons as Queues & scheduler)
    buttons: [
      { label: t("btn.ok"), primary: true, busy: t("btn.saving"), fn: async el => { await save(el); el.close(); toast(t("o.saved")); } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
      { label: t("btn.apply"), busy: t("btn.saving"), fn: async el => { await save(el); el.note(t("o.savedNote")); } },
    ],
  });
  const showTab = name => {
    $$(".otab", el).forEach(x => { const on = x.dataset.t === name; x.classList.toggle("active", on); x.setAttribute("aria-current", on ? "page" : "false"); });
    $$(".pane", el).forEach(x => x.classList.toggle("active", x.dataset.p === name));
    $(".opts-sel", el).value = name;
  };
  const reveal = id => { const x = id && $("#" + id, el); if (x) x.scrollIntoView({ block: "start" }); };
  if (el._showTab) { el._showTab(tab); reveal(focus); return; } // already open: keep what's being edited, just switch section
  el._showTab = showTab;
  $$(".otab", el).forEach(x => x.onclick = () => showTab(x.dataset.t));
  $(".opts-sel", el).onchange = e => showTab(e.target.value);
  showTab(tab);
  (isShown($(".opts-nav", el)) ? $(".otab.active", el) : $(".opts-sel", el)).focus(); // start on the section list
  setTimeout(() => reveal(focus), 0);
  $("#o-sums", el).checked = st.checksums !== false; $("#o-refresh", el).checked = st.autoRefresh !== false;
  // torrents
  const tor = st.torrent || {};
  const sharing = tor.seedRatio > 0 && tor.seedMins > 0;
  $(`input[name=o-ts][value=${sharing ? "share" : "off"}]`, el).checked = true;
  $("#o-tratio", el).value = tor.seedRatio > 0 ? tor.seedRatio : 1;
  const tmins = tor.seedMins > 0 ? tor.seedMins : 120;
  $("#o-tmins", el).innerHTML = [...new Set([30, 60, 120, 240, 480, 1440, 2880, tmins])].sort((a, b) => a - b).map(n => `<option value="${n}">${fmtIn(n * 60)}</option>`).join("");
  $("#o-tmins", el).value = tmins;
  const tsync = () => $("#o-tsub", el).setAttribute("aria-disabled", String($("input[name=o-ts]:checked", el).value !== "share"));
  $$("input[name=o-ts]", el).forEach(r => r.onchange = tsync); tsync();
  $("#o-tup", el).value = tor.upKB || 0;
  const tpeers = $("#o-tpeers", el), tp = tor.maxPeers || 60;
  if (![...tpeers.options].some(o => +o.value === tp)) tpeers.append(new Option(String(tp), String(tp)));
  tpeers.value = tp;
  $("#o-tport", el).value = tor.port || "";
  $("#o-tdht", el).checked = !tor.noDHT; $("#o-tbrowser", el).checked = !tor.noBrowser;
  $("#o-tcheck", el).checked = !!tor.checkDone;
  $("#o-tmap", el).checked = !tor.noPortMap;
  // the torrent port: whether the router opened it, and whether torrents go through the proxy
  const drawTNet = v => {
    const mp = v.map || {}, stt = $("#o-tmapst", el);
    const vars = { port: String(mp.port || v.port || ""), ext: String(mp.extPort || ""), via: mp.via || "", ip: mp.external || "", error: mp.error || "" };
    const [text, tone] = ({
      idle: () => [t("o.tMapIdle"), ""], looking: () => [t("o.tMapLooking"), ""], proxy: () => [t("o.tMapProxy"), ""],
      open: () => [mp.extPort ? t("o.tMapOpenAs", vars) : mp.external ? t("o.tMapOpen", vars) : t("o.tMapOpenNoIP", vars), " good"],
      none: () => [t("o.tMapNone"), " bad"], refused: () => [t("o.tMapRefused", vars), " bad"], conflict: () => [t("o.tMapConflict", vars), " bad"],
      cgnat: () => [t("o.tMapCGNAT"), " bad"], private: () => [t("o.tMapPrivate", vars), " bad"],
    }[mp.state] || (() => ["", ""]))();
    stt.className = "hint indent-chk ph-st" + tone;
    stt.textContent = text;
    stt.hidden = !text;
    const px = $("#o-tproxy", el);
    px.hidden = !v.proxy;
    if (v.proxy) $("div", px).textContent = t(v.proxy === "http" ? "o.tProxyNoteHTTP" : "o.tProxyNote");
  };
  const loadTNet = async (first) => {
    if (!el.isConnected) return;
    if (first || $(".pane[data-p=torrents].active", el)) { try { drawTNet(await api("torrent/net")); } catch {} }
    setTimeout(loadTNet, 2000);
  };
  loadTNet(true);
  $("#o-tenc", el).value = ["require", "plain"].includes(tor.enc) ? tor.enc : "";
  let assocWas = null;
  api("torrent/assoc", {}).then(r => {
    if (!el.isConnected) return;
    const mac = S.os === "darwin";
    $("#o-tassocrow", el).hidden = !r.supported;
    if (r.supported) { assocWas = !!r.on; $("#o-tassoc", el).checked = assocWas; }
    $("#o-tassochint", el).textContent = r.supported
      ? t(S.os === "windows" ? "o.tAssocWin" : "o.tAssocOther")
      : t(mac ? "o.tAssocMac" : "o.tAssocNone");
  }).catch(() => {});
  el._assoc = () => assocWas;
  $("#o-prog", el).checked = st.showProgressDialog; $("#o-done", el).checked = st.showCompleteDialog; $("#o-early", el).checked = st.earlyStart !== false;
  $("#o-awake", el).checked = st.keepAwake !== false;
  $(`input[name=o-start][value=${st.startPaused ? "paused" : "resume"}]`, el).checked = true;
  const keys = st.keys || {};
  $("#o-kzip", el).value = keys.zipline || "alt"; $("#o-kbr", el).value = keys.browser || "ctrlshift";
  $("#o-skip", el).value = (st.skipSites || []).join("\n");
  const br = st.browser || {}; // (the browser extension's own settings)
  $("#o-takeover", el).checked = !!br.takeover; $("#o-minkb", el).value = br.minKB ?? 256; $("#o-skiptypes", el).value = br.skipTypes ?? "";
  $("#o-vbtn", el).checked = br.videoButton !== false; $("#o-nobtn", el).value = (br.noButton || []).join("\n");
  // sites
  const siteRow = r => {
    const row = h(`<div class="site-row">
      <div class="site-top"><input class="inp" data-k="site" spellcheck="false" placeholder="${te("o.sitePh")}"><button class="btn sm icon" data-del title="${te("o.siteRemove")}" aria-label="${te("o.siteRemove")}">${I.trash}</button></div>
      <div class="site-grid">
        <label><span>${te("o.siteConns")}</span><select class="inp" data-k="conns"><option value="0">${te("o.usual", { n: st.conns })}</option>${[1, 2, 3, 4, 6, 8, 12, 16, 24, 32].map(n => `<option value="${n}">${n}</option>`).join("")}</select></label>
        <label><span>${te("f.user")}</span><input class="inp" data-k="user" autocomplete="off" spellcheck="false"></label>
        <label><span>${te("f.password")}</span>${passBox(`<input class="inp" data-k="pass" type="password" autocomplete="new-password">`)}</label>
      </div></div>`);
    row.dataset.id = r.id || "";
    $("[data-k=site]", row).value = r.site || ""; $("[data-k=conns]", row).value = String(r.maxConns || 0);
    $("[data-k=user]", row).value = r.user || ""; passInit($("[data-k=pass]", row), r.passSet);
    $("[data-del]", row).onclick = () => { row.remove(); syncSites(); };
    return row;
  };
  const syncSites = () => {
    const list = $("#o-sites", el);
    if (!$(".site-row", list) && !$(".site-empty", list)) list.appendChild(h(`<div class="site-empty">${te("o.siteEmpty")}</div>`));
    if ($(".site-row", list)) $(".site-empty", list)?.remove();
  };
  const drawSites = () => { const list = $("#o-sites", el); list.textContent = ""; (st.sites || []).forEach(r => list.appendChild(siteRow(r))); syncSites(); };
  drawSites();
  $("#o-siteadd", el).onclick = () => { const row = siteRow({}); $("#o-sites", el).appendChild(row); syncSites(); $("[data-k=site]", row).focus(); };
  const drawVault = () => { $("#o-sitehint", el).innerHTML = $("#o-pvault", el).innerHTML = vaultHint(st.vault); };
  drawVault();
  // sounds
  const snd = st.sounds || { done: true, failed: true, queue: true };
  $("#o-snd", el).checked = !!snd.on;
  for (const k of ["done", "failed", "queue"]) $(`#o-snd-${k}`, el).checked = snd[k] !== false;
  const syncSnd = () => $("#o-sndopts", el).setAttribute("aria-disabled", !$("#o-snd", el).checked);
  $("#o-snd", el).onchange = syncSnd;
  $$("[data-play]", el).forEach(b => b.onclick = () => playChime(b.dataset.play, true));
  // data plan
  const plan = st.plan || {};
  $("#o-plan", el).checked = !!plan.on; $("#o-pfrom", el).value = plan.start || "00:00"; $("#o-pto", el).value = plan.end || "07:00";
  $$("[data-pd]", el).forEach(c => c.checked = (plan.days || [0, 1, 2, 3, 4, 5, 6]).includes(+c.dataset.pd));
  $("#o-pmargin", el).value = String(plan.margin ?? 5);
  const syncPlan = () => $("#o-plansub", el).setAttribute("aria-disabled", !$("#o-plan", el).checked);
  $("#o-plan", el).onchange = syncPlan; syncPlan();
  $("#o-meter", el).checked = !!plan.metered;
  $("#o-meter", el).disabled = S.os === "darwin";
  $("#o-meterhint", el).textContent = t(S.os === "windows" ? "o.meterWin" : S.os === "linux" ? "o.meterLinux" : "o.meterMac");
  // phone
  const ph = st.phone || {};
  $("#o-phone", el).checked = !!ph.on; $("#o-phport", el).value = ph.port || 17891;
  $("#o-phfw", el).textContent = t(S.os === "windows" ? "o.phFwWin" : S.os === "darwin" ? "o.phFwMac" : "o.phFwOther");
  const syncPhone = () => $("#o-phonesub", el).setAttribute("aria-disabled", !$("#o-phone", el).checked);
  $("#o-phone", el).addEventListener("change", syncPhone); syncPhone();
  let pairing = null;
  const drawPhone = v => {
    const stt = $("#o-phst", el), sx = phoneState(v); // (phoneui.js)
    stt.className = "hint indent ph-st" + sx.tone;
    stt.textContent = sx.text;
    stt.hidden = !stt.textContent;
    phoneNets($("#o-phnets", el), v, loadPhone);
    const devs = (v.devices || []).filter(d => !d.pending);
    const html = phoneAskRows(v.devices || []) + devs.map(d => `<div class="dev-row" data-id="${esc(d.id)}"><div class="dev-name"><b>${esc(d.name)}</b><span class="hint" style="margin:0">${te("o.phPaired", { added: fmtDate(d.added), seen: fmtAgo(d.seen) })}</span></div><button class="btn sm" data-rm>${te("btn.remove")}</button></div>`).join("")
      || `<div class="site-empty">${ht("o.phNone")}</div>`;
    const box = $("#o-devs", el);
    if (box._h !== html) {
      box.innerHTML = html; box._h = html;
      wirePhoneAsks(box, loadPhone);
      $$("[data-rm]", box).forEach(b => b.onclick = () => {
        const row = b.closest(".dev-row");
        confirmDialog(t("o.phRemoveTitle"), th("o.phRemoveBody", { name: nm(row.querySelector("b").textContent) }), t("btn.remove"), async () => { await api("phone", { action: "remove", id: row.dataset.id }); loadPhone(); });
      });
    }
    if (pairing) phoneAsked($("#o-pwait", el), v, pairing);
    if (pairing && !(v.devices || []).some(d => d.id === pairing.id && d.pending)) {
      const got = (v.devices || []).find(d => d.id === pairing.id);
      $("#o-pwait", el).innerHTML = got ? `<span class="ok">${ht("o.phDone", { name: nm(got.name) })}</span>` : te("o.phGone");
      pairing = null;
    }
    const sel = $("#o-paddr", el);
    $("#o-paddrrow", el).hidden = (v.addrs || []).length < 2;
    const opts = (v.addrs || []).map(a => `<option>${a}</option>`).join("");
    if (sel._h !== opts) { sel.innerHTML = opts; sel._h = opts; }
  };
  const loadPhone = async () => { try { drawPhone(await api("phone")); } catch {} };
  const pairNow = async addr => {
    const r = await api("phone", { action: "pair", addr }).catch(e => ({ error: errText(e) }));
    if (r.error) { el.error(r.code ? t("o.phErr." + r.code) : cap1(r.error)); loadPhone(); return; }
    el.error("");
    pairing = r;
    $("#o-phone", el).checked = true; syncPhone();
    $("#o-pairbox", el).hidden = false;
    $("#o-qr", el).innerHTML = r.qr; $("#o-phurl", el).textContent = r.url; $("#o-pcode", el).textContent = r.code;
    $("#o-pwait", el).innerHTML = `<span class="spin"></span>${te("o.phWaiting")}`;
    if (addr) $("#o-paddr", el).value = addr;
    loadPhone();
  };
  $("#o-phadd", el).onclick = () => pairNow("");
  $("#o-paddr", el).onchange = e => pairNow(e.target.value);
  loadPhone(); el._phone = setInterval(() => { if ($(".pane[data-p=phone].active", el)) loadPhone(); }, 1500);
  extLinkPane(el); // the extension's connection (extlink.js)
  // sharing the line
  $("#o-polite", el).checked = !!st.polite;
  const multi = st.multi || {};
  $("#o-multi", el).checked = !!multi.on;
  let netUse = new Set(multi.use || []), netAll = !(multi.use || []).length, netList = [];
  const syncMulti = () => $("#o-multisub", el).setAttribute("aria-disabled", !$("#o-multi", el).checked);
  $("#o-multi", el).onchange = syncMulti; syncMulti();
  const kindName = k => t({ wifi: "o.netWifi", ethernet: "o.netEthernet", mobile: "o.netMobile", vpn: "o.netVpn" }[k] || "o.netOther");
  const drawNets = () => {
    const box = $("#o-nets", el);
    if (!netList.length) { box.innerHTML = `<div class="hint" style="margin:0">${te("o.netsNone")}</div>`; return; }
    const rows = netList.map(n => `<div class="net-row" data-n="${esc(n.name)}"><label class="chk"><input type="checkbox"${netAll || netUse.has(n.name) ? " checked" : ""}><span class="net-name"><b>${esc(n.name)}</b><span class="net-meta">${esc(kindName(n.kind))} · ${esc(n.ip)}${n.bad ? ` · <span class="tn-err">${te("o.netNoInternet")}</span>` : ""}</span></span></label><span class="net-speed">${esc(fmtSpeed(n.speed) || t("o.netIdle"))}</span><button class="btn sm" data-test>${te("o.netTest")}</button><span class="result"></span></div>`).join("");
    const single = netList.length < 2 ? `<div class="hint" style="margin:8px 0 0">${te("o.netsOne")}</div>` : "";
    if (box._h !== rows + single) {
      box.innerHTML = rows + single; box._h = rows + single;
      $$(".net-row", box).forEach(r => {
        $("input", r).onchange = e => { if (netAll) { netAll = false; netUse = new Set(netList.map(n => n.name)); } e.target.checked ? netUse.add(r.dataset.n) : netUse.delete(r.dataset.n); };
        $("[data-test]", r).onclick = async () => {
          const out = $(".result", r); out.className = "result"; out.innerHTML = `<span class="spin"></span>`;
          const res = await api("netifs", { name: r.dataset.n }).catch(e => ({ error: String(e.message || e) }));
          out.className = "result " + (res.ok ? "ok" : "bad"); out.textContent = res.ok ? t("o.netOnline", { ms: res.ms < 1 ? 0 : res.ms }) : (res.error || t("o.netNone"));
        };
      });
    } else $$(".net-row", box).forEach(r => { const n = netList.find(x => x.name === r.dataset.n); if (n) $(".net-speed", r).textContent = fmtSpeed(n.speed) || t("o.netIdle"); });
  };
  const loadNets = async () => { try { netList = (await api("netifs")).adapters || []; drawNets(); } catch {} };
  loadNets(); el._nets = setInterval(() => { if ($(".pane[data-p=conn].active", el)) loadNets(); }, 1500);
  try { $("#o-theme", el).value = localStorage.getItem("zl.theme") || ""; } catch {}
  $("#o-conns", el).value = String(st.conns); if ($("#o-conns", el).value !== String(st.conns)) $("#o-conns", el).add(new Option(st.conns));
  $("#o-maxact", el).value = String(st.maxActive || 6); if ($("#o-maxact", el).value !== String(st.maxActive || 6)) $("#o-maxact", el).add(new Option(st.maxActive));
  $("#o-lim", el).checked = st.speedLimitOn; $("#o-limkb", el).value = st.speedLimitKB;
  const win = S.os === "windows";
  $("#o-tray", el).checked = st.tray !== false;
  $("#o-tray-row", el).hidden = $("#o-tray-hint", el).hidden = !win;
  const cap = st.cap || { mb: 500, hours: 1 };
  $("#o-cap", el).checked = !!cap.on; $("#o-capmb", el).value = cap.mb || 500; $("#o-caph", el).value = String(cap.hours || 1);
  const capUse = () => {
    const c = S.cap;
    $("#o-capuse", el).textContent = c && c.on ? t("o.capUse", { used: fmtSize(c.used), limit: fmtSize(c.limit), blocked: c.blocked ? "yes" : "no" }) : t("o.capIdle");
  };
  capUse(); el._draw = () => { capUse(); planSum(); quotaUse(); };
  const planSum = () => {
    const out = $("#o-plansum", el), p = S.plan;
    if (!out) return;
    out.hidden = !(p && p.on);
    if (out.hidden) return;
    out.innerHTML = planSummaryHTML(p);
  };
  const sc = st.scan || {};
  $("#o-scan", el).checked = !!sc.on;
  $("#o-scanmode", el).value = sc.program ? "custom" : "auto";
  $("#o-scanprog", el).value = sc.program || ""; $("#o-scanargs", el).value = sc.args || "%f";
  $("#o-scanauto", el).textContent = t(win ? "o.scanWin" : "o.scanOther");
  const syncScan = () => { const custom = $("#o-scanmode", el).value === "custom"; $("#o-scancustom", el).hidden = !custom; $("#o-scanauto", el).hidden = custom; };
  $("#o-scanmode", el).onchange = syncScan; syncScan(); syncSnd();
  $("#o-goq", el).onclick = e => { e.preventDefault(); queueManager(); };
  const px = st.proxy || { mode: "system" };
  const pmode = () => ($$("[name=o-pm]", el).find(r => r.checked) || {}).value || "system";
  const syncProxyUI = () => $("#o-pmanual", el).setAttribute("aria-disabled", pmode() !== "manual");
  $$("[name=o-pm]", el).forEach(r => { r.checked = r.value === (px.mode || "system"); r.onchange = syncProxyUI; });
  const drawProxy = () => { const p = st.proxy || {}; $("#o-purl", el).value = p.url || ""; passInit($("#o-ppass", el), p.passSet); };
  $("#o-puser", el).value = px.user || ""; $("#o-pbyp", el).value = px.bypass || "<local>";
  drawProxy();
  syncProxyUI();
  const readProxy = () => ({ mode: pmode(), url: $("#o-purl", el).value.trim(), user: $("#o-puser", el).value.trim(), ...passRead($("#o-ppass", el)), bypass: $("#o-pbyp", el).value.trim() });
  $("#o-ptest", el).onclick = async () => {
    const res = $("#o-pres", el); res.className = "result"; res.innerHTML = `<span class="spin"></span>${te("o.pTesting")}`;
    try {
      const r = await api("proxytest", readProxy());
      res.className = "result " + (r.ok ? "ok" : "bad");
      res.textContent = r.ok ? t("o.pOk", { via: r.via }) : t("o.pFail", { via: r.via, error: r.error });
    } catch (e) { res.className = "result bad"; res.textContent = String(e.message || e); }
  };
  $("#o-dir", el).value = st.downloadDir; $("#o-catf", el).checked = st.categoryFolders; $("#o-clip", el).checked = st.clipboardMonitor;
  $("#o-cats", el).innerHTML = st.categories.map((c, i) => field(`${catIcon(c.name).replace("flex:none", "display:inline-block;vertical-align:-3px;margin-right:6px")}${esc(catName(c.name))}`, `<input class="inp" id="o-cat${i}" data-i="${i}" value="${esc(c.exts.join(" "))}" spellcheck="false">`)).join("");
  $("#o-browse", el).onclick = () => pickFolder($("#o-dir", el));
  // traffic profiles and their schedule
  const tr = st.traffic || {};
  const ATONCE = [1, 2, 3, 4, 5, 6, 8, 10, 12, 15, 20, 30, 50], CONNS = [1, 2, 4, 6, 8, 12, 16, 24, 32];
  const usualOpt = (list, v, usual) => `<option value="0">${te("o.usual", { n: usual })}</option>` + [...new Set([...list, v])].filter(n => n > 0).sort((a, b) => a - b).map(n => `<option value="${n}">${n}</option>`).join("");
  const profRows = () => $$("#o-tprofs .prof-row", el);
  const profNames = () => profRows().map(r => ({ id: r.dataset.id, name: $("[data-k=name]", r).value.trim() || t("o.profDefault") }));
  const inUse = () => (S.profile && S.profile.id) || tr.default;
  const syncProfiles = () => { // the choices follow the names; the one in use (or the last one) can't be deleted
    const ps = profNames();
    for (const sel of [$("#o-tpuse", el), ...$$("#o-tprules [data-k=profile]", el)]) {
      const v = sel.value;
      sel.innerHTML = ps.map(p => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join("");
      sel.value = ps.some(p => p.id === v) ? v : ps[0]?.id || "";
    }
    profRows().forEach(r => { const b = $("[data-del]", r), lock = r.dataset.id === inUse() || ps.length < 2; b.disabled = lock; b.title = lock ? t(ps.length < 2 ? "o.profLast" : "o.profInUse") : t("o.profDelete"); });
    $("#o-tpusel", el).textContent = t($("#o-tpsched", el).checked ? "o.tpOtherwise" : "o.tpUse");
  };
  const profRow = p => {
    const row = h(`<div class="site-row prof-row">
      <div class="site-top"><input class="inp" data-k="name" spellcheck="false" maxlength="40" aria-label="${te("o.profName")}"><button class="btn sm icon" data-del aria-label="${te("o.profDelete")}">${I.trash}</button></div>
      <div class="site-grid prof-grid">
        <label><span>${te("o.profDown")}</span><span class="inline"><input class="inp num" data-k="down" type="number" min="0" step="64" placeholder="${te("o.noLimit")}"><span class="unit">${te("unit.kilobyte-per-second")}</span></span></label>
        <label><span>${te("o.profUp")}</span><span class="inline"><input class="inp num" data-k="up" type="number" min="0" step="16" placeholder="${te("o.noLimit")}"><span class="unit">${te("unit.kilobyte-per-second")}</span></span></label>
        <label><span>${te("o.profAtOnce")}</span><select class="inp" data-k="atOnce">${usualOpt(ATONCE, p.atOnce || 0, st.maxActive || 6)}</select></label>
        <label><span>${te("o.profConns")}</span><select class="inp" data-k="conns">${usualOpt(CONNS, p.conns || 0, st.conns)}</select></label>
      </div>
      <label class="chk" style="margin-bottom:0"><input type="checkbox" data-k="seed"> ${te("o.profSeed")}</label></div>`);
    row.dataset.id = p.id || "p" + Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
    $("[data-k=name]", row).value = p.name || ""; $("[data-k=down]", row).value = p.downKB || ""; $("[data-k=up]", row).value = p.upKB || "";
    $("[data-k=atOnce]", row).value = String(p.atOnce || 0); $("[data-k=conns]", row).value = String(p.conns || 0); $("[data-k=seed]", row).checked = !p.noSeed;
    $("[data-k=name]", row).oninput = syncProfiles;
    $("[data-del]", row).onclick = () => { row.remove(); syncProfiles(); };
    return row;
  };
  const ruleRow = r => {
    const row = h(`<div class="rule-row">
      <select class="inp" data-k="profile" aria-label="${te("o.ruleProfile")}"></select>
      <span class="inline rule-times"><input class="inp narrow" type="time" data-k="from" aria-label="${te("o.ruleFrom")}"><span class="unit">${te("o.planTo")}</span><input class="inp narrow" type="time" data-k="to" aria-label="${te("o.ruleTo")}"></span>
      <button class="btn sm icon" data-del title="${te("o.ruleRemove")}" aria-label="${te("o.ruleRemove")}">${I.trash}</button>
      <div class="days">${DAYS.map((d, i) => `<label><input type="checkbox" data-d="${i}">${esc(d)}</label>`).join("")}</div></div>`);
    $("[data-k=from]", row).value = r.from || "09:00"; $("[data-k=to]", row).value = r.to || "18:00";
    $$("[data-d]", row).forEach(c => c.checked = (r.days || [1, 2, 3, 4, 5]).includes(+c.dataset.d));
    $("[data-del]", row).onclick = () => { row.remove(); syncRules(); };
    row._want = r.profile;
    return row;
  };
  const syncRules = () => {
    const list = $("#o-tprules", el);
    if (!$(".rule-row", list) && !$(".site-empty", list)) list.appendChild(h(`<div class="site-empty">${te("o.ruleEmpty")}</div>`));
    if ($(".rule-row", list)) $(".site-empty", list)?.remove();
  };
  (tr.profiles || []).forEach(p => $("#o-tprofs", el).appendChild(profRow(p)));
  (tr.rules || []).forEach(r => $("#o-tprules", el).appendChild(ruleRow(r)));
  $("#o-tpsched", el).checked = !!tr.sched;
  syncProfiles(); syncRules();
  $("#o-tpuse", el).value = tr.default || "";
  $$("#o-tprules .rule-row", el).forEach(r => { $("[data-k=profile]", r).value = r._want; });
  const syncSched = () => { $("#o-tpschedsub", el).setAttribute("aria-disabled", !$("#o-tpsched", el).checked); syncProfiles(); };
  $("#o-tpsched", el).onchange = syncSched; syncSched();
  $("#o-tpadd", el).onclick = () => { const row = profRow({ name: "" }); $("#o-tprofs", el).appendChild(row); syncProfiles(); $("[data-k=name]", row).focus(); };
  $("#o-tpruleadd", el).onclick = () => { const light = profNames().find(p => p.id !== $("#o-tpuse", el).value); const row = ruleRow({ profile: light && light.id }); $("#o-tprules", el).appendChild(row); syncProfiles(); $("[data-k=profile]", row).value = row._want || $("[data-k=profile]", row).value; syncRules(); };
  const usual = () => profRows().forEach(r => { $("[data-k=atOnce] option[value='0']", r).textContent = t("o.usual", { n: $("#o-maxact", el).value }); $("[data-k=conns] option[value='0']", r).textContent = t("o.usual", { n: $("#o-conns", el).value }); });
  $("#o-maxact", el).addEventListener("change", usual); $("#o-conns", el).addEventListener("change", usual);
  // data quotas
  const qs = st.quota || {};
  $("#o-qcount", el).value = qs.count || "both"; $("#o-qwarn", el).value = String(qs.warn || 80);
  $("#o-upfree", el).checked = !(st.plan || {}).upPaid;
  const netOpts = q => {
    const qv = S.quota || {}, opts = [["", t("o.qEvery"), ""]];
    if (qv.netKey) opts.push([qv.netKey, t("o.qThis", { name: qv.netName }), qv.netName]);
    for (const n of qv.nets || []) if (n.key !== qv.netKey) opts.push([n.key, n.name || n.key, n.name || n.key]);
    if (q.network && !opts.some(o => o[0] === q.network)) opts.push([q.network, q.netName || q.network, q.netName || q.network]);
    return opts.map(([k, label, name]) => `<option value="${esc(k)}" data-name="${esc(name)}">${esc(label)}</option>`).join("");
  };
  const quotaRow = q => {
    const gb = q.mb >= 1024 && q.mb % 1024 === 0 || q.mb >= 10240 || !q.mb;
    const row = h(`<div class="site-row quota-row">
      <div class="site-top"><select class="inp" data-k="net" aria-label="${te("o.qNetAria")}">${netOpts(q)}</select><button class="btn sm icon" data-del title="${te("o.qRemove")}" aria-label="${te("o.qRemove")}">${I.trash}</button></div>
      <div class="site-grid quota-grid">
        <label><span>${te("o.qAllowance")}</span><span class="inline"><input class="inp num" data-k="amount" type="number" min="0.1" step="any"><select class="inp" data-k="unit" aria-label="${te("o.qUnit")}"><option value="1024">${te("o.qGB")}</option><option value="1">${te("o.qMB")}</option></select></span></label>
        <label><span>${te("o.qPeriod")}</span><select class="inp" data-k="period"><option value="day">${te("o.qDay")}</option><option value="month">${te("o.qMonth")}</option></select></label>
        <label data-reset><span>${te("o.qReset")}</span><select class="inp" data-k="reset">${Array.from({ length: 31 }, (_, i) => `<option value="${i + 1}">${te("o.qDayN", { n: i + 1 })}</option>`).join("")}</select></label>
      </div>
      <div class="quota-use"></div></div>`);
    row.dataset.id = q.id || "";
    $("[data-k=net]", row).value = q.network ?? (S.quota && S.quota.netKey) ?? "";
    $("[data-k=unit]", row).value = gb ? "1024" : "1"; $("[data-k=amount]", row).value = q.mb ? +(q.mb / (gb ? 1024 : 1)).toFixed(2) : 2;
    $("[data-k=period]", row).value = q.period || "month"; $("[data-k=reset]", row).value = String(q.resetDay || 1);
    const syncP = () => $("[data-reset]", row).hidden = $("[data-k=period]", row).value !== "month";
    $("[data-k=period]", row).onchange = syncP; syncP();
    $("[data-del]", row).onclick = () => { row.remove(); syncQuotas(); };
    return row;
  };
  const syncQuotas = () => {
    const list = $("#o-quotas", el);
    if (!$(".quota-row", list) && !$(".site-empty", list)) list.appendChild(h(`<div class="site-empty">${te("o.qEmpty")}</div>`));
    if ($(".quota-row", list)) $(".site-empty", list)?.remove();
  };
  (qs.rules || []).forEach(q => $("#o-quotas", el).appendChild(quotaRow(q)));
  syncQuotas();
  $("#o-qadd", el).onclick = () => { const row = quotaRow({}); $("#o-quotas", el).appendChild(row); syncQuotas(); $("[data-k=amount]", row).focus(); };
  const quotaUse = () => { // how much of each saved quota is used, and "carry on anyway"
    const qv = S.quota || {};
    $("#o-qnet", el).innerHTML = qv.netName ? ht("o.qNet", { net: nm(qv.netName) }) : "";
    for (const row of $$(".quota-row", el)) {
      const v = (qv.all || []).find(x => x.id === row.dataset.id), out = $(".quota-use", row);
      const html = !v ? "" : `<span>${v.applies ? ht("o.qUsed", { period: quotaPeriod(v), used: fmtSize(v.used), limit: fmtSize(v.limit), state: v.over ? (v.carryOn ? "carry" : "over") : "none", resets: quotaResets(v) }) : te("o.qNotHere")}</span>${v.applies && v.over && !v.carryOn ? `<button class="btn sm" data-carry>${te("o.qCarry")}</button>` : ""}`;
      if (out._h !== html) {
        out.innerHTML = html; out._h = html;
        const b = $("[data-carry]", out); if (b) b.onclick = async () => { const r = await api("quota", { id: v.id }).catch(e => ({ error: errText(e) })); if (r.error) el.error(cap1(r.error)); };
      }
    }
  };
  quotaUse();
  // torrents: sharing at once, the file-list wait
  $("#o-tmaxseeds", el).value = String(tor.maxSeeds || 5); if ($("#o-tmaxseeds", el).value !== String(tor.maxSeeds || 5)) $("#o-tmaxseeds", el).add(new Option(String(tor.maxSeeds), String(tor.maxSeeds)));
  const meta = tor.metaMins || 15; $("#o-tmeta", el).value = String(meta); if ($("#o-tmeta", el).value !== String(meta)) $("#o-tmeta", el).add(new Option(fmtIn(meta * 60), String(meta)));
  // bad shows what's wrong on its field (opening that section first) and stops the save
  const bad = (ctl, msg, tab) => {
    if (tab) showTab(tab);
    if (typeof ctl === "string") ctl = $(ctl, el);
    fieldError(ctl, msg);
    (ctl.matches("input,select,textarea,button") ? ctl : $("input,select,textarea,button", ctl) || ctl).focus();
    throw new Invalid();
  };
  async function save(el) {
    st.showProgressDialog = $("#o-prog", el).checked; st.showCompleteDialog = $("#o-done", el).checked; st.earlyStart = $("#o-early", el).checked;
    st.keepAwake = $("#o-awake", el).checked; st.startPaused = $("input[name=o-start]:checked", el).value === "paused";
    st.keys = { zipline: $("#o-kzip", el).value, browser: $("#o-kbr", el).value };
    if (st.keys.zipline !== "off" && st.keys.zipline === st.keys.browser) bad("#o-kbr", t("o.errKeys"), "integration");
    st.skipSites = $("#o-skip", el).value.split(/\n+/).map(x => x.trim()).filter(Boolean);
    const minKB = Math.round(+$("#o-minkb", el).value);
    if (!(minKB >= 0)) bad("#o-minkb", t("o.errMinSize"), "integration");
    st.browser = { takeover: $("#o-takeover", el).checked, minKB, skipTypes: $("#o-skiptypes", el).value, videoButton: $("#o-vbtn", el).checked,
      noButton: $("#o-nobtn", el).value.split(/\n+/).map(x => x.trim().replace(/^\w+:\/\//, "").replace(/\/.*$/, "")).filter(Boolean) };
    st.sites = [];
    for (const row of $$("#o-sites .site-row", el)) {
      const r = { site: $("[data-k=site]", row).value.trim(), maxConns: +$("[data-k=conns]", row).value || 0, user: $("[data-k=user]", row).value.trim(), id: row.dataset.id, ...passRead($("[data-k=pass]", row)) };
      if (!r.site && !r.user && !r.maxConns) continue;
      if (!r.site) bad($("[data-k=site]", row), t("o.errSite"), "sites");
      st.sites.push(r);
    }
    st.plan = { ...(st.plan || {}), on: $("#o-plan", el).checked, start: $("#o-pfrom", el).value || "00:00", end: $("#o-pto", el).value || "07:00",
      days: $$("[data-pd]", el).filter(c => c.checked).map(c => +c.dataset.pd), margin: +$("#o-pmargin", el).value, metered: $("#o-meter", el).checked };
    if (st.plan.on && !st.plan.days.length) bad("#o-pdays", t("o.errDays"), "plan");
    if (st.plan.on && st.plan.start === st.plan.end && st.plan.days.length === 7) bad("#o-ptime", t("o.errAllDay"), "plan");
    st.polite = $("#o-polite", el).checked;
    const port = Math.round(+$("#o-phport", el).value || 17891);
    if (port < 1024 || port > 65535 || port === 17890) bad("#o-phport", t("o.errPort"), "phone");
    st.phone = { on: $("#o-phone", el).checked, port };
    st.checksums = $("#o-sums", el).checked; st.autoRefresh = $("#o-refresh", el).checked;
    const share = $("input[name=o-ts]:checked", el).value === "share", ratio = +$("#o-tratio", el).value;
    if (share && !(ratio > 0)) bad("#o-tratio", t("o.errRatio"), "torrents");
    const tport = $("#o-tport", el).value.trim() === "" ? 0 : Math.round(+$("#o-tport", el).value);
    if (tport && (!(tport >= 1024 && tport <= 65535) || tport === 17890 || tport === port)) bad("#o-tport", t("o.errTPort"), "torrents");
    st.torrent = { ...(st.torrent || {}), port: tport, maxPeers: +$("#o-tpeers", el).value, upKB: Math.max(0, Math.round(+$("#o-tup", el).value || 0)),
      seedRatio: share ? Math.round(ratio * 100) / 100 : 0, seedMins: +$("#o-tmins", el).value, noDHT: !$("#o-tdht", el).checked, noBrowser: !$("#o-tbrowser", el).checked };
    st.torrent.checkDone = $("#o-tcheck", el).checked;
    st.multi = { on: $("#o-multi", el).checked, use: netAll ? [] : [...netUse] };
    if (st.multi.on && !netAll && !netUse.size) bad("#o-multi", t("o.errNets"), "conn");
    st.sounds = { on: $("#o-snd", el).checked, done: $("#o-snd-done", el).checked, failed: $("#o-snd-failed", el).checked, queue: $("#o-snd-queue", el).checked };
    st.conns = +$("#o-conns", el).value;
    st.maxActive = +$("#o-maxact", el).value;
    st.proxy = readProxy();
    if (st.proxy.mode === "manual" && !st.proxy.url) bad("#o-purl", t("o.errProxy"), "proxy");
    st.speedLimitOn = $("#o-lim", el).checked; st.speedLimitKB = Math.max(1, +$("#o-limkb", el).value || 1024);
    st.tray = $("#o-tray", el).checked;
    st.cap = { on: $("#o-cap", el).checked, mb: Math.max(1, +$("#o-capmb", el).value || 500), hours: +$("#o-caph", el).value || 1 };
    const custom = $("#o-scanmode", el).value === "custom", scanWas = el._scanWas ??= { ...(st.scan || {}) }; // (as last saved)
    st.scan = { on: $("#o-scan", el).checked, program: custom ? $("#o-scanprog", el).value.trim() : "", args: custom ? $("#o-scanargs", el).value.trim() : "" };
    if (custom && !st.scan.program) bad("#o-scanprog", t("o.errScanner"), "scan");
    st.downloadDir = $("#o-dir", el).value.trim(); st.categoryFolders = $("#o-catf", el).checked; st.clipboardMonitor = $("#o-clip", el).checked;
    if (!st.downloadDir) bad("#o-dir", t("o.errDir"), "save");
    $$("#o-cats input", el).forEach(inp => st.categories[+inp.dataset.i].exts = inp.value.toLowerCase().split(/[\s,;.]+/).filter(Boolean));
    // traffic profiles
    const profiles = profRows().map(r => ({ id: r.dataset.id, name: $("[data-k=name]", r).value.trim(), downKB: Math.max(0, Math.round(+$("[data-k=down]", r).value || 0)),
      upKB: Math.max(0, Math.round(+$("[data-k=up]", r).value || 0)), atOnce: +$("[data-k=atOnce]", r).value, conns: +$("[data-k=conns]", r).value, noSeed: !$("[data-k=seed]", r).checked }));
    const unnamed = profRows().find(r => !$("[data-k=name]", r).value.trim());
    if (unnamed) bad($("[data-k=name]", unnamed), t("o.errProfName"), "conn");
    const rules = $$("#o-tprules .rule-row", el).map(r => ({ profile: $("[data-k=profile]", r).value, from: $("[data-k=from]", r).value, to: $("[data-k=to]", r).value, days: $$("[data-d]", r).filter(c => c.checked).map(c => +c.dataset.d) }));
    const badRule = $$("#o-tprules .rule-row", el).find((r, i) => !rules[i].from || !rules[i].to || !rules[i].days.length);
    if ($("#o-tpsched", el).checked && badRule) bad($("[data-k=from]", badRule), t("o.errTimes"), "conn");
    st.traffic = { ...tr, profiles, rules, default: $("#o-tpuse", el).value, sched: $("#o-tpsched", el).checked };
    // data quotas
    const quotaRows = $$(".quota-row", el), quotas = [];
    for (const r of quotaRows) {
      const amount = +$("[data-k=amount]", r).value, net = $("[data-k=net]", r);
      if (!(amount > 0)) bad($("[data-k=amount]", r), t("o.errQuota"), "plan");
      const name = net.value ? net.selectedOptions[0].dataset.name : "";
      quotas.push({ id: r.dataset.id, on: true, network: net.value, netName: name, period: $("[data-k=period]", r).value, resetDay: +$("[data-k=reset]", r).value, mb: Math.max(1, Math.round(amount * +$("[data-k=unit]", r).value)) });
    }
    st.quota = { count: $("#o-qcount", el).value, warn: +$("#o-qwarn", el).value, rules: quotas };
    st.plan.upPaid = !$("#o-upfree", el).checked;
    st.torrent.maxSeeds = +$("#o-tmaxseeds", el).value; st.torrent.metaMins = +$("#o-tmeta", el).value;
    st.torrent.noPortMap = !$("#o-tmap", el).checked;
    st.torrent.enc = $("#o-tenc", el).value;
    // everything checked: now change things (the theme and the torrent file association too, so Cancel never leaves half a save)
    if (st.scan.program && (st.scan.program !== (scanWas.program || "") || st.scan.args !== (scanWas.args || "")) && !(await confirmProgram(st.scan.program, st.scan.args))) { showTab("scan"); throw new Invalid(); }
    const assocNow = assocWas !== null && $("#o-tassoc", el).checked !== assocWas;
    if (assocNow) {
      const r = await api("torrent/assoc", { on: $("#o-tassoc", el).checked }).catch(e => ({ error: errText(e) }));
      if (r.error) bad("#o-tassoc", t("o.errAssoc", { error: r.error }), "torrents");
      assocWas = $("#o-tassoc", el).checked;
    }
    st.theme = $("#o-theme", el).value;
    try { S.settings = await api("settings", st); }
    catch (e) { if (/proxy/i.test(String(e.message))) bad("#o-purl", errText(e), "proxy"); throw e; }
    setTheme($("#o-theme", el).value);
    Object.keys(st).forEach(k => delete st[k]); Object.assign(st, S.settings); // go on from what the app now holds
    el._scanWas = { ...(st.scan || {}) };
    drawSites(); drawProxy(); drawVault(); // (the password boxes go back to "saved", the new rows have their ids)
  }
}

// ---- When finished: a queue's action, "When all downloads finish", the countdown; carrying on at start-up
const WHEN_IDS = ["none", "quit", "sleep", "hibernate", "shutdown"];
const whenLabel = a => WHEN_IDS.includes(a) ? t("wd." + a) : a;
// powerCan: this computer can do it (hibernating, say, isn't offered everywhere)
const powerCan = a => a === "none" || !S.power || (S.power.can || []).includes(a);
const whenOptions = cur => WHEN_IDS.filter(v => powerCan(v) || v === cur).map(v => `<option value="${v}">${te("wd." + v)}</option>`).join("");
function whenDoneItems() {
  const cur = (S.power && S.power.whenAllDone) || "none";
  return WHEN_IDS.filter(powerCan).map(v => menuItem(t("wd." + v), "", () => setWhenAllDone(v), { checked: v === cur }));
}
async function setWhenAllDone(v) {
  try { await api("power", { action: "whenDone", value: v }); } catch (e) { return toast(String(e.message || e)); }
  toast(t(v === "none" ? "wd.toastNone" : "wd.toast", { action: v }), 3500);
}
const POWER_KEEP = { quit: "wd.keepQuit", sleep: "wd.keepSleep", hibernate: "wd.keepOn", shutdown: "wd.keepOn" };
// powerUI shows the countdown before a "when finished" action, and the start-up countdown.
function powerUI() {
  const p = S.power || {}, pend = p.pending;
  let el = $('.dlg[data-key="power"]');
  if (pend && pend.at) {
    if (!el) {
      // an alert dialog that starts on Cancel: nothing happens unless you choose it
      el = dialog({ title: pend.why, width: 420, key: "power", alert: true, focus: "#pw-cancel",
        body: `<div class="pw-count"><div class="pw-big" id="pw-big" role="timer"></div><div class="hint"></div></div>`,
        buttons: [
          { label: t("wd.now"), danger: true, fn: el => { el._done = true; el.close(); api("power", { action: "now" }).catch(() => {}); } },
          { label: t("btn.cancel"), id: "pw-cancel", cancel: true, fn: el => el.close() },
        ],
        onClose: () => { if (!el._done) api("power", { action: "cancel" }).then(() => toast(t("wd.cancelled"))).catch(() => {}); },
      });
    }
    el.setAttribute("aria-describedby", "pw-big");
    const left = Math.max(0, Math.ceil((pend.at - Date.now()) / 1000));
    $(".dlg-h .t", el).textContent = pend.why; // (a stronger action may have taken over)
    $(".pw-big", el).innerHTML = ht("wd.countdown", { text: pend.text, wait: fmtSecs(left) });
    $(".hint", el).textContent = POWER_KEEP[pend.action] ? t(POWER_KEEP[pend.action]) : "";
  } else if (el) { el._done = true; el.close(); }
  resumeUI(p.resume);
}
function resumeUI(r) {
  let bar = $("#resumebar");
  if (!r) { closeToast(bar); resumeUI.hidden = false; return; }
  const what = [r.downloads ? t("dl.count", { n: r.downloads }) : "", r.sharing ? t("dl.sharing", { n: r.sharing }) : "", ...(r.queues || []).map(nm)].filter(Boolean);
  const text = th("wd.resuming", { list: listOf(what), wait: fmtSecs(Math.max(0, Math.ceil((r.at - Date.now()) / 1000))) });
  if (bar) put($(".t", bar), text);
  else if (!resumeUI.hidden) { // (closed with × it stays closed until the next time)
    toast(text, { id: "resumebar", sticky: true, quiet: true, onDismiss: () => { resumeUI.hidden = true; },
      action: { label: t("btn.cancel"), fn: () => { resumeUI.hidden = true; api("power", { action: "resumeCancel" }).catch(() => {}); toast(t("wd.staysPaused")); } } });
    say(t("wd.pressCancel", { text: String(text) }));
  }
}

// ---- Queues & scheduler
const DAYS = WEEKDAYS; // (Sunday first, like Date.getDay())
const newQueueModel = n => ({ id: "q" + Date.now().toString(36), name: t("qm.defaultName", { n }), maxConcurrent: 2, sync: false, retries: 3, retryMins: 5,
  schedule: { enabled: false, start: "02:00", stopEnabled: false, stop: "07:00", days: [0, 1, 2, 3, 4, 5, 6], whenDone: "none", repeatEnabled: false, repeatMins: 120, wake: false } });
async function queueManager(opts = {}) {
  const st = await api("settings");
  let queues = JSON.parse(JSON.stringify(st.queues));
  let cur = opts.select || queueTarget() || queues[0].id;
  if (!queues.some(q => q.id === cur)) cur = queues[0].id;
  let qsel = new Set();
  const el = dialog({
    title: t("qm.title"), width: 860, key: "sched",
    body: `<div class="qm">
      <div class="qm-nav">
        <div class="qm-list" id="qm-list" role="listbox" aria-label="${te("qm.queues")}"></div>
        <div class="inline"><button class="btn sm" id="qm-new">${I.plus}<span>${te("qm.new")}</span></button><button class="btn sm" id="qm-del">${te("btn.delete")}</button></div>
      </div>
      <div class="qm-main">
        ${field(te("f.name"), `<input class="inp" id="qm-name" maxlength="40">`)}
        ${field(te("fact.type"), `<div class="seg" role="radiogroup" aria-labelledby="qm-type-l"><label><input type="radio" name="qm-type" value="dl"><span>${te("qm.typeDl")}</span></label><label><input type="radio" name="qm-type" value="sync"><span>${te("qm.typeSync")}</span></label></div>`, { group: true, noRole: true, id: "qm-type", hint: "", hintId: "qm-type-hint" })}
        ${field(te("qm.max"), `<input class="inp num" type="number" id="qm-max" min="1" max="20">`)}
        ${field(te("qm.fail"), `<div class="inline"><span class="group"><span class="unit">${te("qm.tryAgain")}</span><input class="inp num" type="number" id="qm-retries" min="0" max="20" aria-label="${te("qm.triesAria")}"><span class="unit">${te("qm.times")}</span></span><span class="group"><span class="unit">${te("qm.every")}</span><input class="inp num" type="number" id="qm-retrymins" min="1" max="1440" aria-label="${te("qm.minsAria")}"><span class="unit">${te("qm.minutes")}</span></span></div>`, { group: true, id: "qm-fail", hint: te("qm.failHint") })}
        <div class="section-title"><span>${te("qm.schedule")}</span></div>
        <div class="plan-sum" id="qm-plannote" hidden></div>
        <div id="qm-schedbox"><div class="hint" id="qm-timehint" style="margin:0 0 12px"></div><div class="sched-grid">
          <label class="chk"><input type="checkbox" id="qm-on"> ${te("qm.startAt")}</label><input class="inp narrow" type="time" id="qm-start" aria-label="${te("qm.startAria")}">
          <div id="qm-wakerow" style="display:contents" hidden><span></span><div><label class="chk"><input type="checkbox" id="qm-wake"> ${te("qm.wake")}</label><div class="hint indent-chk" style="margin-bottom:0">${te("qm.wakeHint")}</div></div></div>
          <label class="chk"><input type="checkbox" id="qm-rep"> ${te("qm.repeat")}</label>
          <div class="inline"><input class="inp num" type="number" min="1" id="qm-repn" aria-label="${te("qm.repeatAria")}"><select class="inp narrow" id="qm-repu" aria-label="${te("qm.unitAria")}"><option value="1">${te("qm.unitMin")}</option><option value="60">${te("qm.unitHr")}</option></select></div>
          <label class="chk"><input type="checkbox" id="qm-stopon"> ${te("qm.stopAt")}</label><input class="inp narrow" type="time" id="qm-stop" aria-label="${te("qm.stopAria")}">
          <span class="lbl" id="qm-days-l">${te("qm.days")}</span><div class="days" id="qm-days" role="group" aria-labelledby="qm-days-l">${DAYS.map((d, i) => `<label><input type="checkbox" data-d="${i}">${esc(d)}</label>`).join("")}</div>
          <label class="lbl" for="qm-done">${te("qm.whenDone")}</label><select class="inp" id="qm-done"></select>
          <span></span><div class="hint" style="margin:0">${te("qm.whenDoneHint")}</div>
        </div></div>
        <div class="section-title"><span>${te("qm.files")}</span><span class="hint" id="qm-cnt"></span></div>
        <div class="qlist" id="qm-files"></div>
        <div class="inline" style="margin-top:8px"><button class="btn sm" id="qm-up">${te("ctx.moveUp")}</button><button class="btn sm" id="qm-down">${te("ctx.moveDown")}</button><button class="btn sm" id="qm-rm">${te("ctx.removeFromQueue")}</button></div>
      </div></div>`,
    left: `<button class="btn" id="qm-run">${te("qm.run")}</button><button class="btn" id="qm-halt">${te("qm.halt")}</button>`,
    // edits stay in this window until they are saved: OK saves and closes, Cancel throws them away, Apply saves
    // and stays (the same buttons as Options)
    buttons: [
      { label: t("btn.ok"), primary: true, busy: t("btn.saving"), fn: async el => { await save(); el.close(); toast(t("qm.saved")); } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
      { label: t("btn.apply"), busy: t("btn.saving"), fn: async el => { await save(); el.note(t("qm.savedNote")); } },
    ],
  });
  const q = () => queues.find(x => x.id === cur);
  const typeHint = () => $("#qm-type-hint", el).textContent = t($("[name=qm-type][value=sync]", el).checked ? "qm.hintSync" : "qm.hintDl");
  function writeForm() {
    const x = q(), sc = x.schedule;
    $("#qm-name", el).value = x.name;
    $$("[name=qm-type]", el).forEach(r => r.checked = (r.value === "sync") === !!x.sync);
    $("#qm-max", el).value = x.maxConcurrent;
    $("#qm-retries", el).value = x.retries ?? 3; $("#qm-retrymins", el).value = x.retryMins || 5;
    $("#qm-on", el).checked = sc.enabled; $("#qm-start", el).value = sc.start;
    $("#qm-wake", el).checked = !!sc.wake;
    $("#qm-stopon", el).checked = sc.stopEnabled; $("#qm-stop", el).value = sc.stop;
    $("#qm-rep", el).checked = sc.repeatEnabled;
    const hrs = sc.repeatMins % 60 === 0; $("#qm-repu", el).value = hrs ? "60" : "1"; $("#qm-repn", el).value = hrs ? sc.repeatMins / 60 : sc.repeatMins;
    $("#qm-done", el).innerHTML = whenOptions(sc.whenDone); $("#qm-done", el).value = sc.whenDone;
    $$("[data-d]", el).forEach(c => c.checked = (sc.days || []).includes(+c.dataset.d));
    const planQ = x.id === PLAN_Q && S.plan && S.plan.on;
    $("#qm-del", el).disabled = x.id === "main" || planQ;
    $("#qm-del", el).title = planQ ? t("qm.planTip") : "";
    $("#qm-schedbox", el).hidden = planQ; $("#qm-plannote", el).hidden = !planQ;
    typeHint(); syncEnabled(); qsel.clear(); draw(true);
  }
  function readForm() {
    const x = q(); if (!x) return;
    x.name = $("#qm-name", el).value.trim() || x.name;
    x.sync = $("[name=qm-type][value=sync]", el).checked;
    x.maxConcurrent = Math.min(20, Math.max(1, +$("#qm-max", el).value || 1));
    x.retries = Math.min(20, Math.max(0, Math.round(+$("#qm-retries", el).value || 0)));
    x.retryMins = Math.min(1440, Math.max(1, Math.round(+$("#qm-retrymins", el).value || 5)));
    x.schedule = { enabled: $("#qm-on", el).checked, start: $("#qm-start", el).value || "02:00", stopEnabled: $("#qm-stopon", el).checked, stop: $("#qm-stop", el).value || "07:00",
      days: $$("[data-d]", el).filter(c => c.checked).map(c => +c.dataset.d), whenDone: $("#qm-done", el).value,
      repeatEnabled: $("#qm-rep", el).checked, repeatMins: Math.max(1, (+$("#qm-repn", el).value || 1) * +$("#qm-repu", el).value), wake: $("#qm-wake", el).checked };
  }
  function syncEnabled() {
    $("#qm-start", el).disabled = $("#qm-wake", el).disabled = !$("#qm-on", el).checked;
    $("#qm-retrymins", el).disabled = !(+$("#qm-retries", el).value > 0);
    $("#qm-stop", el).disabled = !$("#qm-stopon", el).checked;
    $("#qm-repn", el).disabled = $("#qm-repu", el).disabled = !$("#qm-rep", el).checked;
  }
  function renderList() {
    const counts = {}; S.downloads.forEach(d => { if (d.queue) counts[d.queue] = (counts[d.queue] || 0) + 1; });
    $("#qm-list", el).innerHTML = queues.map(x => {
      const live = S.queues.find(z => z.id === x.id);
      return `<div class="qn${x.id === cur ? " active" : ""}" data-id="${x.id}" role="option" aria-selected="${x.id === cur}">${x.sync ? I.syncQ : I.listQ}<span>${esc(x.name)}</span>${live && qBusy(live) ? `<i class="run-dot" title="${te("sb.running")}"></i>` : ""}<span class="cnt">${counts[x.id] || 0}</span></div>`;
    }).join("");
    $$(".qn", el).forEach(n => n.onclick = () => { readForm(); cur = n.dataset.id; renderList(); writeForm(); });
  }
  function draw(force) {
    if (!el.isConnected) return;
    const files = S.downloads.filter(d => d.queue === cur);
    $("#qm-cnt", el).textContent = files.length ? t("qm.count", { n: files.length }) : "";
    const html = files.map(d => `<div class="qitem${qsel.has(d.id) ? " sel" : ""}" data-id="${d.id}">${catIcon(d.category)}<span style="overflow:hidden;text-overflow:ellipsis"><bdi>${esc(dname(d))}</bdi></span><span class="s">${esc(d.syncNote && d.status === "completed" ? d.syncNote : statusText(d))}</span></div>`).join("")
      || `<div class="hint" style="padding:10px">${ht("qm.empty", { later: t("fi.later"), add: th("a.addTo", { name: nm(q()?.name || "queue") }) })}</div>`;
    const L = $("#qm-files", el);
    if (force || L._h !== html) {
      L.innerHTML = html; L._h = html;
      $$(".qitem", L).forEach(n => n.onclick = e => { if (!e.ctrlKey && !e.metaKey) qsel.clear(); qsel.has(n.dataset.id) ? qsel.delete(n.dataset.id) : qsel.add(n.dataset.id); draw(true); });
    }
    const live = S.queues.find(z => z.id === cur);
    if (cur === PLAN_Q && S.plan && S.plan.on) {
      const note = `<div>${ht("qm.planNote", { window: planWindowText(S.plan), link: raw(`<a href="#" data-goplan>${te("qm.planLink")}</a>`) })}</div><div>${esc(planEstimateText(S.plan))}</div>`;
      const n = $("#qm-plannote", el);
      if (n._h !== note) { n.innerHTML = note; n._h = note; const a = $("[data-goplan]", n); if (a) a.onclick = e => { e.preventDefault(); optionsDialog("plan"); }; }
    }
    $("#qm-run", el).disabled = live ? qBusy(live) : false;
    $("#qm-halt", el).disabled = !(live && qBusy(live));
  }
  // check marks the first thing that can't be saved in the queue being edited
  function check() {
    const bad = (sel, msg) => { const i = $(sel, el); fieldError(i, msg); i.focus(); throw new Invalid(); };
    const whole = (sel, lo, hi) => { const i = $(sel, el), v = +i.value; if (!i.disabled && (i.value.trim() === "" || !Number.isInteger(v) || v < lo || v > hi)) bad(sel, t("qm.whole", { lo, hi })); };
    if (!$("#qm-name", el).value.trim()) bad("#qm-name", t("qm.errName"));
    whole("#qm-max", 1, 20); whole("#qm-retries", 0, 20); whole("#qm-retrymins", 1, 1440); whole("#qm-repn", 1, 99999);
    if (!$("#qm-schedbox", el).hidden && $("#qm-on", el).checked && !$$("[data-d]", el).some(c => c.checked)) bad("#qm-days", t("qm.errDays"));
  }
  async function save() {
    check(); readForm();
    const fresh = await api("settings");
    fresh.queues = queues;
    S.settings = await api("settings", fresh);
    queues = JSON.parse(JSON.stringify(S.settings.queues));
    if (!queues.some(x => x.id === cur)) cur = queues[0].id;
    renderList(); writeForm(); // (show what the app now holds)
  }
  el._draw = () => { draw(); if (!el._listT || Date.now() - el._listT > 1500) { el._listT = Date.now(); renderList(); } };
  ["#qm-on", "#qm-stopon", "#qm-rep"].forEach(id => $(id, el).onchange = syncEnabled);
  $("#qm-retries", el).oninput = syncEnabled;
  // waking needs a wake timer, which only Windows lets an app set (macOS and Linux need administrator setup)
  $("#qm-wakerow", el).hidden = S.os !== "windows";
  $("#qm-timehint", el).textContent = [t("qm.timeHint"), S.os === "windows" ? "" : t("qm.timeHintOs")].filter(Boolean).join(" ");
  $$("[name=qm-type]", el).forEach(r => r.onchange = typeHint);
  $("#qm-name", el).oninput = () => { const n = $(`.qn[data-id="${cur}"] span`, el); if (n) n.textContent = $("#qm-name", el).value || "…"; };
  // a new or deleted queue is part of the edits like any other: it exists (or goes) when you press OK or Apply
  $("#qm-new", el).onclick = () => {
    readForm();
    const nq = newQueueModel(queues.length + 1);
    queues.push(nq); cur = nq.id;
    renderList(); writeForm(); $("#qm-name", el).focus(); $("#qm-name", el).select();
  };
  $("#qm-del", el).onclick = () => {
    const x = q(); if (!x || x.id === "main") return;
    confirmDialog(th("qm.delTitle"), th("qm.delBody", { name: nm(x.name) }), t("btn.delete"), () => {
      queues = queues.filter(z => z.id !== x.id); cur = queues[0].id;
      renderList(); writeForm();
    });
  };
  // (a refused move or stop shows on the window's error line, not as an unhandled error)
  const qact = (action, ids) => Promise.resolve(act(action, ids)).catch(e => el.fail(e));
  $("#qm-up", el).onclick = () => qact("up", [...qsel]);
  $("#qm-down", el).onclick = () => qact("down", [...qsel]);
  $("#qm-rm", el).onclick = () => { qact("queueRemove", [...qsel]); qsel.clear(); };
  $("#qm-run", el).onclick = e => el.run(e.currentTarget, async () => { await save(); await startQueue(cur); }, t("fi.starting"));
  $("#qm-halt", el).onclick = e => el.run(e.currentTarget, () => api("queue", { action: "stop", id: cur }), t("qm.stopping"));
  renderList(); writeForm();
  if (opts.create) $("#qm-new", el).click();
}
const schedDialog = () => queueManager();

// ---- Site grabber
const GRAB_TYPES = ["Images", "Video", "Music", "Documents", "Compressed", "Programs"];
async function grabberDialog(mode = {}) {
  const open = document.querySelector(`dialog.dlg[data-key="grab"]`);
  if (open && open._scanFrom && (mode.url || mode.session)) { // "Scan this site" from the browser while the window is open
    raiseDialog(open); open._scanFrom(mode); mode.onClose && mode.onClose(); return;
  }
  const st = S.settings || await api("settings");
  let projects = await api("grab/projects").catch(() => []);
  let scans = await api("grab/scans").catch(() => []);
  let jobId = null, poll = null, files = [], sel = new Set(), autoSel = true, state = "", projId = "", session = mode.session || "";
  const fromPage = !!mode.jobId; // "Download all links" from the browser extension
  // links from the browser come without cookies: Download asks it for those of the chosen files' sites (a few seconds at most)
  let browserJob = fromPage;
  const dlBtn = { label: t("g.dl"), id: "g-dl", busy: t(fromPage ? "g.cookieWait" : "fi.adding"), fn: () => addSelected() };
  // modeless: a scan runs for minutes, so the window stays beside the list while you carry on with your downloads
  const el = dialog({
    title: t(fromPage ? "g.titlePage" : "a.siteGrabber"), width: 820, key: "grab", modal: false,
    body: `
      <div class="g-setup">
        ${field(te("g.projects"), `<div class="inline"><select class="inp" id="g-proj" style="flex:1 1 200px;width:auto"></select><button class="btn sm" id="g-run">${te("g.run")}</button><button class="btn sm" id="g-pdel">${te("btn.delete")}</button></div>`, { attrs: `id="g-projrow"` })}
        ${field(te("g.scans"), `<div class="inline"><select class="inp" id="g-scan" style="flex:1 1 200px;width:auto;min-width:0"></select><button class="btn sm" id="g-scanopen">${te("g.scanOpen")}</button><button class="btn sm" id="g-scanres">${te("g.resume")}</button><button class="btn sm" id="g-scandel">${te("btn.delete")}</button></div>`, { attrs: `id="g-scanrow"` })}
        ${field(te("g.startLink"), `<input class="inp" id="g-url" spellcheck="false" placeholder="${te("g.urlPh")}">`)}
        ${field(te("g.follow"), `<select class="inp fit" id="g-depth">
          <option value="0">${te("g.depth0")}</option>${[1, 2, 3, 4, 5].map(n => `<option value="${n}"${n === 1 ? " selected" : ""}>${te("g.depthN", { n })}</option>`).join("")}</select>`)}
        ${field(te("g.stay"), `<div class="seg" role="radiogroup" aria-labelledby="g-scope-l">
          <label><input type="radio" name="g-scope" value="site" checked><span>${te("g.scopeSite")}</span></label><label><input type="radio" name="g-scope" value="domain"><span>${te("g.scopeDomain")}</span></label><label><input type="radio" name="g-scope" value="folder"><span>${te("g.scopeFolder")}</span></label></div>`, { group: true, noRole: true, id: "g-scope" })}
        ${field(te("g.types"), `<div class="chips">${GRAB_TYPES.map(c => `<label><input type="checkbox" data-t="${c}"${c === "Images" ? " checked" : ""}>${catIcon(c)}${esc(catName(c))}</label>`).join("")}</div>
          <input class="inp" id="g-exts" style="margin-top:8px" spellcheck="false" placeholder="${te("g.extsPh")}" aria-label="${te("g.extsAria")}">`, { top: true, group: true, id: "g-types" })}
        ${field(te("g.options"), `<label class="chk" style="margin:0 0 6px"><input type="checkbox" id="g-cross" checked> ${te("g.cross")}</label>
          <label class="chk" style="margin:0 0 6px"><input type="checkbox" id="g-robots" checked> ${te("g.robots")}</label>
          <label class="chk" style="margin:0"><input type="checkbox" id="g-sitemap" checked> ${te("g.sitemap")}</label>`, { top: true, group: true, id: "g-optsg" })}
        ${field(te("g.stopAfter"), `<div class="inline"><input class="inp num" id="g-max" type="number" min="1" max="5000" value="300"><span class="unit">${te("g.pages")}</span></div>`)}
        ${field(te("g.wait"), `<div class="inline"><input class="inp num" id="g-wait" type="number" min="0.05" max="60" step="0.05" value="0.25"><span class="unit">${te("g.waitUnit")}</span></div>`, { hint: te("g.waitHint") })}
        ${headerFields("g-")}
      </div>
      <div class="g-results" hidden>
        <div class="grab-status"><span id="g-spin" class="spin"></span><span id="g-stat"></span><span class="grow"></span><button class="btn sm" id="g-stop">${te("g.stop")}</button></div>
        <div class="grab-notes">
          <div class="info-box" id="g-n-slow" hidden><div></div></div>
          <div class="info-box" id="g-n-js" hidden><div></div><button class="btn sm" id="g-collect"></button></div>
          <div class="info-box" id="g-n-resume" hidden><div></div><button class="btn sm" id="g-resume">${te("g.resume")}</button></div>
          <details class="fold" id="g-n-fail" hidden><summary></summary><div class="fold-body"><div class="grab-fails" id="g-fails"></div></div></details>
        </div>
        <div class="grab-tools">
          <input class="inp" id="g-filter" placeholder="${te("g.filterPh")}" spellcheck="false">
          <select class="inp" id="g-type"><option value="">${te("g.allTypes")}</option></select>
          <span class="inline" style="gap:4px"><button class="btn sm" id="g-all">${te("g.selectAll")}</button><button class="btn sm" id="g-none">${te("g.none")}</button></span>
          <span class="unit" id="g-count"></span>
        </div>
        <div class="grab-list" id="g-list"></div>
        <div class="sub">
          ${field(te("g.saveTo"), pickControl("g-dir", "g-browse"))}
          ${field(te("g.addTo"), `<div class="inline">
            <select class="inp fit" id="g-q">${S.queues.map(q => `<option value="${q.id}">${esc(q.name)}</option>`).join("")}</select>
            <label class="chk" style="margin:0"><input type="checkbox" id="g-go" checked> ${te("g.startNow")}</label>
            <label class="chk" style="margin:0"><input type="checkbox" id="g-keep"> ${te("g.keep")}</label></div>`, { group: true, id: "g-addto" })}
        </div>
        <div class="section-title g-proj-sec"><span>${te("g.projSave")}</span><span class="hint">${te("g.projHint")}</span></div>
        <div class="g-proj-sec">
          ${field(te("g.projName"), `<div class="pick"><input class="inp" id="g-pname"><button class="btn browse" id="g-psave">${te("g.projBtn")}</button></div>`)}
          ${field(te("g.every"), `<div class="inline"><input class="inp num" id="g-every" type="number" min="0" value="0"><span class="unit">${te("g.everyUnit")}</span></div>`)}
        </div>
      </div>`,
    left: `<button class="btn" id="g-back" hidden>${te(fromPage ? "g.scanInstead" : "g.back")}</button>`,
    buttons: [
      { label: t("g.start"), primary: true, id: "g-start", busy: t("fi.starting"), fn: () => start() },
      dlBtn,
      { label: t("btn.close"), cancel: true, fn: el => el.close() },
    ],
    onClose: () => {
      clearInterval(poll); if (state === "running" && jobId) api("grab/stop", { id: jobId });
      if (jobId && browserJob) api("browser/closed", { job: jobId }).catch(() => {}); // (the extension can stop listening for it)
      mode.onClose && mode.onClose();
    },
  });
  if (fromPage) $$(".g-proj-sec", el).forEach(x => x.hidden = true);
  const setup = $(".g-setup", el), results = $(".g-results", el);
  const showResults = on => {
    setup.hidden = on; results.hidden = !on; $("#g-back", el).hidden = !on;
    $("#g-start", el).style.display = on ? "none" : ""; $("#g-dl", el).style.display = on ? "" : "none";
    $("#g-dl", el).classList.toggle("primary", on);
  };
  showResults(false);

  function fillProjects() {
    $("#g-projrow", el).hidden = !projects.length;
    $("#g-proj", el).innerHTML = projects.map(p => `<option value="${p.id}">${p.autoMins ? ht(p.autoMins / 60 >= 1 ? "g.projH" : "g.projMin", { name: nm(p.name), n: p.autoMins / 60 >= 1 ? p.autoMins / 60 : p.autoMins }) : esc(p.name)}</option>`).join("");
  }
  fillProjects();
  // earlier scans (kept across restarts): open one, or pick a stopped one up where it left off
  const scanLabel = x => t("g.scanOpt", { host: hostOf(x.url) || x.url, when: fmtAgo(x.started), files: x.files, state: t(x.interrupted ? "g.st.interrupted" : "g.st." + x.state) });
  const curScan = () => scans.find(x => x.id === $("#g-scan", el).value);
  function fillScans() {
    const keep = $("#g-scan", el).value;
    $("#g-scanrow", el).hidden = !scans.length;
    $("#g-scan", el).innerHTML = scans.map(x => `<option value="${x.id}">${esc(scanLabel(x))}</option>`).join("");
    if (scans.some(x => x.id === keep)) $("#g-scan", el).value = keep;
    showScanButtons();
  }
  function showScanButtons() { const x = curScan(); $("#g-scanres", el).hidden = !(x && x.resumable); }
  // "Scan this site" in the browser: the address to start from, and the sign-in the browser sent along
  function fillSignin() {
    const row = $("#g-url", el).closest(".field");
    let hint = $("#g-signin", row);
    if (session && !hint) { hint = h(`<div class="hint" id="g-signin"></div>`); $(".f-err", row).before(hint); }
    if (hint) { hint.hidden = !session; hint.textContent = t("g.signedHint"); }
  }
  el._scanFrom = m => {
    session = m.session || ""; $("#g-url", el).value = m.url || "";
    if (!results.hidden) $("#g-back", el).click();
    fillSignin(); $("#g-url", el).focus();
  };
  function readOpts() {
    return {
      url: $("#g-url", el).value.trim(), depth: +$("#g-depth", el).value,
      scope: ($$("[name=g-scope]", el).find(r => r.checked) || {}).value || "site",
      types: $$(".chips [data-t]", el).filter(c => c.checked).map(c => c.dataset.t),
      exts: $("#g-exts", el).value, crossSite: $("#g-cross", el).checked, robots: $("#g-robots", el).checked, sitemap: $("#g-sitemap", el).checked,
      maxPages: +$("#g-max", el).value || 300, wait: Math.max(50, Math.round((+$("#g-wait", el).value || 0.25) * 1000)), headers: readHeaders(el, "g-"),
    };
  }
  function writeOpts(o) {
    $("#g-url", el).value = o.url || ""; $("#g-depth", el).value = String(o.depth ?? 1);
    $$("[name=g-scope]", el).forEach(r => r.checked = r.value === (o.scope || "site"));
    $$(".chips [data-t]", el).forEach(c => c.checked = (o.types || []).includes(c.dataset.t));
    $("#g-exts", el).value = o.exts || ""; $("#g-cross", el).checked = o.crossSite !== false; $("#g-robots", el).checked = o.robots !== false;
    $("#g-max", el).value = o.maxPages || 300; $("#g-sitemap", el).checked = !!o.sitemap;
    $("#g-wait", el).value = o.wait ? +(o.wait / 1000).toFixed(2) : 0.25;
  }
  const hostOf = u => { try { return new URL(u).hostname; } catch { return ""; } };

  async function start(existingJob) {
    let id = existingJob;
    if (!id) {
      const o = readOpts();
      if (!o.url) { fieldError($("#g-url", el), t("g.errUrl")); $("#g-url", el).focus(); return; }
      if (!o.types.length && !o.exts.trim()) { fieldError($(".chips", el), t("g.errTypes")); $("[data-t]", el).focus(); return; }
      if (session) o.session = session; // the browser's sign-in, from "Scan this site"
      const r = await api("grab/start", o);
      if (r.error) throw new Error(r.error);
      id = r.id;
      projId = "";
    }
    jobId = id; files = []; sel = new Set(); autoSel = true; state = "running";
    const src = fromPage && existingJob ? mode.page || "" : $("#g-url", el).value;
    const host = hostOf(src.includes("://") ? src : "https://" + src);
    if (!$("#g-dir", el).value) $("#g-dir", el).value = joinPath(st.downloadDir, host || "Grabbed");
    if (!$("#g-pname", el).value) $("#g-pname", el).value = host;
    const tq = queueTarget(); if (tq) $("#g-q", el).value = tq;
    showResults(true);
    clearInterval(poll);
    poll = setInterval(refresh, 700);
    refresh();
  }
  async function refresh() {
    if (!el.isConnected) return clearInterval(poll);
    let j;
    try { j = await api("grab/status?id=" + jobId); } catch { clearInterval(poll); return; }
    state = j.state;
    browserJob = j.fromPage || (j.collects || []).some(c => c.state === "done");
    dlBtn.busy = t(browserJob ? "g.cookieWait" : "fi.adding");
    const prevLen = files.length;
    files = j.files || [];
    if (autoSel) for (const f of files.slice(prevLen)) if (!j.fromPage || f.category !== "General") sel.add(f.url);
    const running = j.state === "running";
    $("#g-spin", el).hidden = !running; $("#g-stop", el).hidden = !running;
    const words = ["running", "done", "stopped", "error"].includes(j.state) ? t("g.st." + j.state) : j.state;
    put($("#g-stat", el), j.fromPage
      ? th("g.fromPage", { n: files.length, title: nm(j.title || hostOf(mode.page || "") || t("g.thisPage")) })
      : raw(esc([t("g.scanned", { words, pages: j.pages, files: files.length }), j.skipped ? t("g.skipped", { n: j.skipped }) : "",
        (j.signedIn || []).length ? t("g.signedIn") : "", j.error || ""].filter(Boolean).join(" · "))));
    drawNotes(j);
    const types = [...new Set(files.map(f => f.category))].sort();
    const ts = $("#g-type", el), tv = ts.value, typeHtml = `<option value="">${te("g.allTypes")}</option>` + types.map(c => `<option value="${esc(c)}">${esc(catName(c))}</option>`).join("");
    if (ts._h !== typeHtml) { ts.innerHTML = typeHtml; ts._h = typeHtml; ts.value = tv; }
    if (files.length !== prevLen || !running) drawList();
    if (!running && !(j.collects || []).some(c => c.state === "waiting")) clearInterval(poll);
  }
  // what the scan has to say besides its files: a server asking for patience, pages the browser
  // has to read, pages that failed, a scan to pick up again
  function drawNotes(j) {
    const running = j.state === "running", delays = Object.entries(j.delays || {});
    const slow = $("#g-n-slow", el);
    slow.hidden = !(running && (j.wait || delays.length));
    if (!slow.hidden) put($("div", slow), j.wait ? th("g.slowWait", { n: j.wait.secs, host: nm(j.wait.host) }) : th("g.crawlDelay", { n: delays[0][1], host: nm(delays[0][0]) }));

    const pages = j.jsPages || [], cs = j.collects || [], jsBox = $("#g-n-js", el), btn = $("#g-collect", el);
    const last = u => cs.filter(c => c.page === u).pop(), waiting = cs.some(c => c.state === "waiting");
    const todo = pages.filter(p => !(last(p.url) || {}).state || last(p.url).state === "failed");
    const failed = cs.filter(c => c.state === "failed" && todo.some(p => p.url === c.page)).pop();
    jsBox.hidden = !pages.length;
    btn.hidden = waiting || !todo.length; btn.textContent = t(failed ? "g.collectAgain" : "g.collect");
    if (waiting) $("div", jsBox).innerHTML = `<span class="spin" aria-hidden="true"></span>${ht("g.collectWaiting")}`;
    else if (todo.length) $("div", jsBox).innerHTML = ht("g.jsNote", { n: todo.length }) + (failed ? ` <span class="hint">${ht("g.collectFail", { why: t(["timeout", "empty", "blocked"].includes(failed.error) ? "g.collectFail." + failed.error : "g.collectFail.other") })}</span>` : "");
    else if (pages.length) put($("div", jsBox), th("g.collectDone", { n: cs.filter(c => c.state === "done").reduce((a, c) => a + c.links, 0) }));

    const fails = j.failures || [], box = $("#g-n-fail", el);
    box.hidden = !j.failed;
    if (j.failed) {
      put($("summary", box), th("g.failedN", { n: j.failed }));
      const why = f => f.kind === "status" ? t("g.failStatus", { status: String(f.status), text: f.detail || "" }) : f.kind === "robots" ? t("g.failRobots", { why: f.detail || "" }) : (f.detail || t("g.failNet"));
      const html = fails.map(f => `<div class="grab-fail"><span class="u">${esc(f.url)}</span><span class="why">${esc(why(f))}</span></div>`).join("");
      if ($("#g-fails", el)._h !== html) { $("#g-fails", el).innerHTML = html; $("#g-fails", el)._h = html; }
    }
    const res = $("#g-n-resume", el);
    res.hidden = !(j.resumable && !running);
    if (!res.hidden) put($("div", res), th(j.interrupted ? "g.interrupted" : "g.leftN", { n: j.left }));
  }
  function filtered() {
    const q = $("#g-filter", el).value.toLowerCase(), type = $("#g-type", el).value;
    return files.filter(f => (!type || f.category === type) && (!q || (f.name + " " + f.url + " " + (f.text || "")).toLowerCase().includes(q)));
  }
  function drawList() {
    const L = filtered(), shown = L.slice(0, 2000);
    $("#g-list", el).innerHTML = shown.map((f, i) => `<div class="grow-row" data-i="${i}" title="${esc(f.url)}">
        <input type="checkbox"${sel.has(f.url) ? " checked" : ""} aria-label="${esc(f.name || f.url)}">${catIcon(f.category)}<span class="n">${esc(f.name || f.url)}</span>
        <span class="from">${esc(f.text && f.text !== f.name ? f.text : f.url.replace(/^\w+:\/\//, ""))}</span><span class="x">${esc(f.ext || t("g.page"))}</span></div>`).join("")
      + (L.length > shown.length ? `<div class="grab-empty">${te("g.showingFirst", { shown: 2000, total: L.length })}</div>` : "")
      || `<div class="grab-empty">${te(state === "running" ? "g.looking" : "g.noFiles")}</div>`;
    $$(".grow-row", el).forEach(r => r.onclick = () => {
      const f = shown[+r.dataset.i]; autoSel = false;
      sel.has(f.url) ? sel.delete(f.url) : sel.add(f.url);
      $("input", r).checked = sel.has(f.url); count();
    });
    count();
  }
  function count() {
    const n = files.filter(f => sel.has(f.url)).length;
    $("#g-count", el).textContent = t("g.selected", { n });
    $("#g-dl", el).textContent = n ? t("g.dlN", { n }) : t("g.dl");
    $("#g-dl", el).disabled = !n;
  }
  async function addSelected() {
    const urls = files.filter(f => sel.has(f.url)).map(f => f.url);
    if (!urls.length) return;
    const qid = $("#g-q", el).value;
    const r = await api("grab/add", { id: jobId, urls, dir: $("#g-dir", el).value.trim(), keep: $("#g-keep", el).checked, queueId: qid, start: $("#g-go", el).checked });
    if (r.error) throw new Error(r.error);
    el.close();
    if (r.note === "nocookies") toast(th("n.addedNoSignin", { n: r.added, queue: nm(qName(qid)) }), { ms: 7000, kind: "warn" });
    else toast(th("n.addedFiles", { n: r.added, queue: nm(qName(qid)) }));
  }

  $("#g-filter", el).oninput = drawList;
  $("#g-type", el).onchange = drawList;
  $("#g-all", el).onclick = () => { autoSel = false; filtered().forEach(f => sel.add(f.url)); drawList(); };
  $("#g-none", el).onclick = () => { autoSel = false; filtered().forEach(f => sel.delete(f.url)); drawList(); };
  $("#g-stop", el).onclick = () => api("grab/stop", { id: jobId });
  $("#g-back", el).onclick = () => {
    clearInterval(poll); if (state === "running") api("grab/stop", { id: jobId });
    if (fromPage) { $("#g-url", el).value = mode.page || ""; $(".t", el).textContent = t("a.siteGrabber"); $$(".g-proj-sec", el).forEach(x => x.hidden = false); }
    showResults(false);
    api("grab/scans").then(x => { scans = x; fillScans(); }, () => {});
  };
  $("#g-browse", el).onclick = () => pickFolder($("#g-dir", el));
  $("#g-psave", el).onclick = e => el.run(e.currentTarget, async () => {
    const hours = Math.max(0, +$("#g-every", el).value || 0), name = $("#g-pname", el).value.trim();
    if (!name) { fieldError($("#g-pname", el), t("g.errName")); $("#g-pname", el).focus(); return; }
    const r = await api("grab/project", { jobId, project: { id: projId, name, opts: readOpts(), queueId: $("#g-q", el).value,
      dir: $("#g-dir", el).value.trim(), keep: $("#g-keep", el).checked, autoMins: Math.round(hours * 60) } });
    if (r.error) throw new Error(r.error);
    projId = r.project.id;
    projects = await api("grab/projects"); fillProjects();
    el.note(hours ? t("g.savedEvery", { n: hours }) : t("g.saved"));
  }, t("btn.saving"));
  const curProject = () => projects.find(p => p.id === $("#g-proj", el).value);
  $("#g-proj", el).onchange = () => { const p = curProject(); if (p) writeOpts(p.opts); };
  $("#g-run", el).onclick = e => el.run(e.currentTarget, async () => {
    const p = curProject(); if (!p) return;
    writeOpts(p.opts);
    $("#g-dir", el).value = p.dir || ""; $("#g-keep", el).checked = !!p.keep; $("#g-q", el).value = p.queueId || "main";
    $("#g-pname", el).value = p.name; $("#g-every", el).value = p.autoMins ? +(p.autoMins / 60).toFixed(2) : 0;
    const r = await api("grab/project/run", { id: p.id });
    if (r.error) throw new Error(r.error);
    projId = p.id;
    await start(r.id);
  }, t("fi.starting"));
  $("#g-pdel", el).onclick = () => {
    const p = curProject(); if (!p) return;
    confirmDialog(th("g.delTitle"), th("g.delBody", { name: nm(p.name) }), t("btn.delete"), async () => {
      await api("grab/project/delete", { id: p.id }); projects = await api("grab/projects"); fillProjects();
    });
  };
  $("#g-scan", el).onchange = showScanButtons;
  // open / resume a saved scan: its options go back in the form, so Back shows what it was started with
  async function openScan(id) {
    const j = await api("grab/status?id=" + id);
    writeOpts(j.opts); projId = "";
    await start(id);
  }
  $("#g-scanopen", el).onclick = e => el.run(e.currentTarget, async () => { const x = curScan(); if (x) await openScan(x.id); }, t("btn.opening"));
  $("#g-scanres", el).onclick = e => el.run(e.currentTarget, async () => {
    const x = curScan(); if (!x) return;
    const r = await api("grab/resume", { id: x.id });
    if (r.error) throw new Error(r.error);
    await openScan(x.id);
  }, t("fi.starting"));
  $("#g-scandel", el).onclick = () => {
    const x = curScan(); if (!x) return;
    confirmDialog(th("g.scanDelTitle"), th("g.scanDelBody", { name: nm(hostOf(x.url) || x.url) }), t("btn.delete"), async () => {
      await api("grab/scan/delete", { id: x.id }); scans = await api("grab/scans"); fillScans();
    });
  };
  $("#g-resume", el).onclick = e => el.run(e.currentTarget, async () => {
    const r = await api("grab/resume", { id: jobId });
    if (r.error) throw new Error(r.error);
    state = "running"; clearInterval(poll); poll = setInterval(refresh, 700); refresh();
  }, t("fi.starting"));
  $("#g-collect", el).onclick = e => el.run(e.currentTarget, async () => {
    const r = await api("grab/browser", { id: jobId });
    if (r.error) throw new Error(r.code === "noext" ? t("g.noExt") : r.error);
    clearInterval(poll); poll = setInterval(refresh, 700); refresh();
  }, t("g.asking"));
  fillScans(); fillSignin();
  if (fromPage) { start(mode.jobId); return; }
  if (mode.url) $("#g-url", el).value = mode.url;
  else if (projects.length) writeOpts(projects[0].opts);
  $("#g-url", el).focus();
}

// ---- Export / import
function exportDialog(ids) {
  const some = Array.isArray(ids) && ids.length > 0;
  const el = dialog({
    title: t("ex.title"), width: 500, key: "export",
    body: `
      <div class="radio-list">
        <label class="radio"><input type="radio" name="x-fmt" value="json" checked><div><b>${te("ex.json")}</b><div class="hint">${te("ex.jsonHint")}</div></div></label>
        <label class="radio"><input type="radio" name="x-fmt" value="txt"><div><b>${te("ex.txt")}</b><div class="hint">${te("ex.txtHint")}</div></div></label>
      </div>
      <div style="margin-top:12px">
        <label class="chk"><input type="checkbox" id="x-some"${some ? " checked" : " disabled"}> ${te(some ? "ex.some" : "ex.someNone", { n: some ? ids.length : 0 })}</label>
        <label class="chk" id="x-sec-row"><input type="checkbox" id="x-sec"> ${te("ex.secrets")}</label>
        <div class="hint indent-chk">${te("ex.secretsHint")}</div>
      </div>`,
    buttons: [
      { label: t("ex.save"), primary: true, busy: t("btn.saving"), fn: el => save(el) },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
  });
  const fmt = () => ($$("[name=x-fmt]", el).find(r => r.checked) || {}).value;
  $$("[name=x-fmt]", el).forEach(r => r.onchange = () => { $("#x-sec-row", el).hidden = fmt() === "txt"; $(".indent-chk", el).hidden = fmt() === "txt"; });
  async function save(el) {
    const q = new URLSearchParams({ format: fmt(), secrets: $("#x-sec", el).checked ? "1" : "0" });
    if (some && $("#x-some", el).checked) q.set("ids", ids.join(","));
    const res = await fetch("/api/export?" + q, { headers: ZH });
    if (!res.ok) throw new Error(await res.text()); // (an error page is not a download list)
    const name = res.headers.get("X-Filename") || "zipline-downloads.json";
    const text = await res.text();
    if (window.showSaveFilePicker) {
      try {
        const handle = await showSaveFilePicker({ suggestedName: name, types: [fmt() === "txt" ? { description: t("ex.descTxt"), accept: { "text/plain": [".txt"] } } : { description: t("ex.descJson"), accept: { "application/json": [".json"] } }] });
        const w = await handle.createWritable(); await w.write(text); await w.close();
        el.close(); toast(th("n.savedName", { name: nm(handle.name) })); return;
      } catch (e) { if (e.name === "AbortError") return; }
    }
    // no save dialog available: put it in the download folder and show it
    const r = await api("export?" + q + "&save=1");
    if (r.error) throw new Error(r.error);
    el.close(); toast(th("n.savedTo", { path: nm(r.path) }), 4000);
  }
}
function importDialog() {
  let content = "", fileName = "";
  const el = dialog({
    title: t("im.title"), width: 520, key: "import",
    body: `
      ${field(te("im.file"), `<div class="pick"><input class="inp" id="i-name" readonly placeholder="${te("im.none")}"><button class="btn browse" id="i-choose">${I.folderOpen}<span>${te("im.choose")}</span></button></div>`, { hint: te("im.fileHint") })}
      <input type="file" id="i-file" accept=".json,.txt,.ef2,.csv,.lst,text/plain,application/json" hidden>
      ${field(te("im.addTo"), `<select class="inp" id="i-q"><option value="">${te("im.listOnly")}</option>${S.queues.map(q => `<option value="${q.id}">${esc(q.name)}</option>`).join("")}</select>`, { hint: te("im.addToHint") })}
      <label class="chk indent-box"><input type="checkbox" id="i-dup" checked> ${te("im.skipDup")}</label>
      <label class="chk indent-box"><input type="checkbox" id="i-start"> ${te("im.startQueue")}</label>`,
    buttons: [
      { label: t("im.go"), primary: true, id: "i-go", busy: t("im.importing"), fn: el => go(el) },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
  });
  $("#i-go", el).disabled = true;
  $("#i-choose", el).onclick = () => $("#i-file", el).click();
  $("#i-name", el).onclick = () => $("#i-file", el).click();
  $("#i-file", el).onchange = async () => {
    const f = $("#i-file", el).files[0]; if (!f) return;
    if (f.size > 30 << 20) { fieldError($("#i-name", el), t("im.tooBig")); return; }
    fileName = f.name; content = await f.text();
    $("#i-name", el).value = f.name; $("#i-go", el).disabled = false; el.error("");
    clearFieldError($("#i-name", el));
  };
  async function go(el) {
    const r = await api("import", { content, queueId: $("#i-q", el).value, start: $("#i-start", el).checked, skipDup: $("#i-dup", el).checked });
    if (r.error) throw new Error(r.error);
    el.close();
    toast(t("im.done", { n: r.added, skipped: r.skipped || 0 }), 4000);
  }
}
// copyText puts text on the clipboard and says so (or says it couldn't) in the window that asked.
async function copyText(el, text, what = t("n.copied")) {
  try { await navigator.clipboard.writeText(text); el.note(what); } catch { el.error(t("n.copyFail")); }
}
async function cliDialog() {
  const el = dialog({
    title: t("menu.cli"), width: 720, key: "cli",
    body: `<div class="hint" style="margin-top:0">${te("cli.hint")}</div><pre class="code" id="cli-text" tabindex="0">${te("btn.loading")}</pre>`,
    left: `<button class="btn" id="cli-copy">${te("btn.copy")}</button>`,
    buttons: [{ label: t("btn.close"), primary: true, cancel: true, fn: el => el.close() }],
  });
  const text = await fetch("/api/cliusage", { headers: ZH }).then(r => r.ok ? r.text() : Promise.reject(new Error(r.statusText))).catch(e => { el.fail(e); return ""; });
  $("#cli-text", el).textContent = text;
  $("#cli-copy", el).onclick = () => copyText(el, text);
  $("#cli-copy", el).disabled = !text;
}

// Help → Diagnostics: version, settings (no passwords) and the recent log, to paste into a bug report.
async function diagDialog() {
  const el = dialog({
    title: t("diag.title"), width: 720, key: "diag",
    body: `<div class="hint" style="margin-top:0">${te("diag.hint")}</div><pre class="code" id="dg-text" tabindex="0">${te("btn.loading")}</pre>`,
    left: `<button class="btn" id="dg-copy">${te("btn.copy")}</button><button class="btn" id="dg-log">${te("diag.log")}</button>`,
    buttons: [{ label: t("btn.close"), primary: true, cancel: true, fn: el => el.close() }],
  });
  const text = await fetch("/api/diagnostics", { headers: ZH }).then(r => r.ok ? r.text() : Promise.reject(new Error(r.statusText))).catch(e => { el.fail(e); return t("diag.fail"); });
  $("#dg-text", el).textContent = text;
  $("#dg-copy", el).onclick = () => copyText(el, text, t("diag.copied"));
  $("#dg-log", el).onclick = e => el.run(e.currentTarget, () => api("diagnostics", {}), t("btn.opening"));
}

// ---- Batch
function batchDialog() {
  const added = new Set(); // what this window has already added, so a second try after a failure doesn't add it again
  const el = dialog({
    title: t("batch.title"), width: 600, key: "batch",
    body: `<div class="tabs" role="tablist" aria-label="${te("batch.tabsAria")}"><button class="tab active" data-t="list" role="tab" id="b-tab-list">${te("batch.tabList")}</button><button class="tab" data-t="pat" role="tab" id="b-tab-pat">${te("batch.tabPat")}</button></div>
      <div class="pane active" data-p="list" role="tabpanel" aria-labelledby="b-tab-list"><textarea class="inp" id="b-list" rows="9" placeholder="${te("batch.listPh")}" aria-label="${te("batch.listAria")}" spellcheck="false"></textarea></div>
      <div class="pane" data-p="pat" role="tabpanel" aria-labelledby="b-tab-pat">
        ${field(te("batch.pat"), `<input class="inp" id="b-pat" placeholder="https://example.com/photos/img*.jpg" spellcheck="false">`)}
        ${field(te("batch.range"), `<div class="row"><input class="inp num" id="b-from" type="number" value="1" aria-label="${te("batch.from")}"><span class="unit">${te("batch.toWord")}</span><input class="inp num" id="b-to" type="number" value="10" aria-label="${te("batch.to")}"><span class="unit">${te("batch.digits")}</span><input class="inp num" id="b-pad" type="number" value="1" min="1" max="8" aria-label="${te("batch.digitsAria")}"></div>`, { group: true, id: "b-range", hint: "", hintId: "b-prev" })}
      </div>
      <div style="margin-top:12px">${headerFields("b-")}</div>
      <div style="margin-top:12px">${field(te("g.addTo"), `<div class="inline"><select class="inp fit" id="b-q">${S.queues.map(q => `<option value="${q.id}">${esc(q.name)}</option>`).join("")}</select>
        <label class="chk" style="margin:0"><input type="checkbox" id="b-go" checked> ${te("g.startNow")}</label></div>`, { group: true, id: "b-addto" })}</div>`,
    buttons: [
      { label: t("batch.add"), primary: true, busy: t("fi.adding"), fn: async (el, btn) => {
        const urls = [...new Set(links(el))]; // each link once, even if it is listed twice
        if (!urls.length) {
          const f = $(mode === "list" ? "#b-list" : "#b-pat", el);
          fieldError(f, t(mode === "list" ? "batch.errList" : "batch.errPat"));
          f.focus(); return;
        }
        const hd = readHeaders(el, "b-"), qid = $("#b-q", el).value;
        let n = 0;
        try {
          for (const u of urls) {
            if (el.closed) break; // (closed while it was adding: stop there)
            n++;
            if (added.has(qid + " " + u)) continue;
            if (btn.lastChild) btn.lastChild.nodeValue = t("batch.adding", { n, total: urls.length });
            await api("add", { url: u, queueId: qid, headers: hd });
            added.add(qid + " " + u);
          }
        } catch (e) { throw new Error(t("batch.fail", { done: n - 1, total: urls.length, error: errText(e).replace(/[.!?]*$/, ".") })); }
        if ($("#b-go", el).checked) startQueue(qid);
        el.close();
        toast(th("batch.done", { n: urls.length, queue: nm(qName(qid)) }));
      } },
      { label: t("btn.cancel"), cancel: true, fn: el => el.close() },
    ],
  });
  let mode = "list";
  function links(el) {
    if (mode === "list") return $("#b-list", el).value.split(/\s+/).map(s => s.trim()).filter(s => URL_RE.test(s));
    const p = $("#b-pat", el).value.trim(), a = +$("#b-from", el).value, b = +$("#b-to", el).value, pad = +$("#b-pad", el).value || 1;
    if (!p.includes("*") || !URL_RE.test(p) || b < a || b - a > 5000) return [];
    const out = []; for (let i = a; i <= b; i++) out.push(p.replace("*", String(i).padStart(pad, "0"))); return out;
  }
  const prev = () => { const L = links(el); $("#b-prev", el).textContent = mode !== "pat" ? "" : L.length ? t("batch.preview", { n: L.length, first: L[0], last: L[L.length - 1] }) : t("batch.star"); };
  ["#b-pat", "#b-from", "#b-to", "#b-pad"].forEach(s => $(s, el).addEventListener("input", prev));
  wireTabs($(".tabs", el), tab => { mode = tab; $$(".pane", el).forEach(p => p.classList.toggle("active", p.dataset.p === tab)); prev(); });
  const tq = queueTarget(); if (tq) $("#b-q", el).value = tq;
}

function aboutDialog() {
  dialog({ title: t("menu.about"), width: 420, key: "about",
    body: `<div style="display:flex;gap:14px;align-items:center">${I.logo.replace(/width="20" height="20"/, 'width="56" height="56"')}<div><div style="font-size:18px;font-weight:600">${te("app.title")}</div><div class="hint" style="margin:2px 0 0">${te("about.version", { version: APP_VERSION })}</div></div></div>
      <p>${te("about.p1")}</p>
      <p class="hint">${te("about.p2")}</p>`,
    buttons: [{ label: t("btn.ok"), primary: true, cancel: true, fn: el => el.close() }] });
}
function shortcutsDialog() {
  const k = [["Ctrl+N", t("a.addLink")], ["Ctrl+V", t("menu.pasteLink")], ["Ctrl+B", t("batch.title")], ["Ctrl+G", t("a.siteGrabber")], ["Ctrl+O", t("sc.openTorrent")], ["Ctrl+A", t("sc.selectAll")], ["Enter", t("sc.openShow")], ["Space", t("sc.pauseResume")], ["Delete", t("a.removeFromList")], ["Shift+Delete", t("a.deleteFile")], ["Alt+Enter", t("sc.properties")], ["Ctrl+F", t("sc.search")],
    [t("sc.altF10"), t("sc.menuBar")], ["Shift+F10", t("sc.contextMenu")], ["↑ ↓ Home End", t("sc.move")], ["Shift+↑ ↓", t("sc.extend")], ["Ctrl+↑ ↓", t("sc.moveNo")], ["Ctrl+Space", t("sc.toggle")]];
  dialog({ title: t("menu.shortcuts"), width: 440, key: "keys", body: `<div class="kv">${k.map(([a, b]) => `<div><b style="color:var(--text)">${esc(a)}</b></div><div>${esc(b)}</div>`).join("")}</div>`, buttons: [{ label: t("btn.ok"), primary: true, cancel: true, fn: el => el.close() }] });
}
function quitApp() {
  const active = S.downloads.some(d => ["downloading", "connecting"].includes(d.status));
  const go = async () => { await api("quit", {}); document.body.innerHTML = `<div style="margin:auto;text-align:center;color:var(--muted)">${I.logo.replace(/width="20" height="20"/, 'width="48" height="48" style="margin:0 auto 10px"')}${te("quit.bye")}</div>`; setTimeout(() => window.close(), 400); };
  if (active) confirmDialog(t("menu.exit"), t("quit.body"), t("quit.exit"), go); else go();
}

// ------------------------------------------------------------------ prompts (clipboard / browser)
async function processPrompts(prompts) {
  if (S.promptBusy) return;
  const p = prompts.find(x => !S.handledPrompts.has(x.id));
  if (!p) return;
  S.handledPrompts.add(p.id); S.promptBusy = true;
  api("prompt/dismiss", { id: p.id });
  window.focus();
  if (p.kind === "ui:add") { S.promptBusy = false; return addUrlDialog(); }
  if (p.kind === "ui:browser") { S.promptBusy = false; return optionsDialog("integration"); } // (the extension's menu: "Browser options…")
  if (p.kind === "links") await new Promise(res => grabberDialog({ jobId: p.jobId, title: p.title, page: p.url, onClose: res }));
  else if (p.kind === "scan") await new Promise(res => grabberDialog({ url: p.url, session: p.jobId, onClose: res })); // (jobId: the browser's sign-in)
  else if (p.kind === "pair") await pairDialog(p); // a browser extension asks to connect (extlink.js)
  else if (p.kind === "phone") await phoneDialog(p); // a phone asks to connect (phoneui.js)
  else await new Promise(res => fileInfoDialog(p.url, p.headers || {}, p.name || "", res, p.title || "", { dir: p.dir, queueId: p.queueId,
    method: p.method, body: p.body, known: p.known, private: p.private, taken: p.taken, promptId: p.id }));
  S.promptBusy = false;
}

// ------------------------------------------------------------------ live updates
// /api/events (events.go) sends the whole list first, then only what changed: "downloads" holds
// the new and changed rows, "removed" the IDs that are gone, "order" the list's order when it
// changed. "seq" counts the events: a gap means one was missed, so the whole list is asked for.
const EV = { client: Math.random().toString(36).slice(2, 10) + Date.now().toString(36), seq: 0, gap: 0, resync: false, t: 0 };
// The open progress windows get their segments (nobody else pays for them); torrents show theirs
// from /api/torrent/detail.
const segIds = () => [...S.progress.keys()].filter(id => !S.byId.get(id)?.torrent);
S.progress = new (class extends Map {
  set(k, v) { super.set(k, v); syncView(); return this; }
  delete(k) { const r = super.delete(k); if (r) syncView(); return r; }
})();
// syncView tells the app what this window needs now: segments, and fewer updates while it's hidden.
function syncView(resync) {
  EV.resync = EV.resync || !!resync;
  clearTimeout(EV.t);
  EV.t = setTimeout(() => {
    const body = { client: EV.client, segs: segIds(), slow: document.hidden, resync: EV.resync };
    EV.resync = false;
    api("events/view", body).catch(() => {});
  }, 20);
}
document.addEventListener("visibilitychange", () => syncView());
// applyList brings S.downloads and S.byId up to date, marking the list in view for a fresh look
// only when a change can move rows in it.
function applyList(s) {
  if (s.full) {
    S.downloads = s.downloads; S.byId = new Map(s.downloads.map(d => [d.id, d]));
    LIST.dirty = true; LIST.pos = null;
    return;
  }
  if (s.removed) {
    const gone = new Set(s.removed);
    S.downloads = S.downloads.filter(d => !gone.has(d.id));
    s.removed.forEach(id => S.byId.delete(id));
    LIST.dirty = true; LIST.pos = null;
  }
  if (s.downloads.length) {
    const pos = LIST.pos || (LIST.pos = new Map(S.downloads.map((d, i) => [d.id, i])));
    for (const d of s.downloads) {
      const was = S.byId.get(d.id);
      S.byId.set(d.id, d);
      if (was) { S.downloads[pos.get(d.id)] = d; if (!LIST.dirty && rowMoves(was, d)) LIST.dirty = true; }
      else { pos.set(d.id, S.downloads.length); S.downloads.push(d); LIST.dirty = true; }
    }
  }
  if (s.order) { S.downloads = s.order.map(id => S.byId.get(id)).filter(Boolean); LIST.dirty = true; LIST.pos = null; }
}
// listState notes what rows show from outside their download (queues, the data cap, free hours…):
// when it changes, the rows on screen are drawn again.
function listState() {
  const g = JSON.stringify([capHeld(), quotaHeld(), S.queues, S.plan && [S.plan.on, S.plan.in, S.plan.metered]]);
  if (g !== LIST.gKey) { LIST.gKey = g; LIST.g++; if (S.sort.col === "queue") LIST.dirty = true; }
}
let es, lostTimer;
function connect() {
  es = new EventSource(`/api/events?client=${EV.client}&segs=${encodeURIComponent(segIds().join(","))}&slow=${document.hidden ? 1 : 0}`);
  es.onopen = () => syncView(); // (an automatic reconnect reuses the first address: say what's needed now)
  es.onmessage = ev => {
    clearTimeout(lostTimer); $("#lost")?.remove();
    const s = JSON.parse(ev.data);
    if (!s.full && (EV.gap || s.seq !== EV.seq + 1)) { if (!EV.gap++ || EV.gap % 5 === 0) syncView(true); return; } // (asks again if it doesn't come)
    EV.seq = s.seq; EV.gap = 0;
    applyList(s);
    S.queueRunning = s.queueRunning; S.totalSpeed = s.totalSpeed; S.limitOn = s.speedLimitOn; S.limitKB = s.speedLimitKB;
    S.queues = s.queues || [];
    S.cap = s.cap; S.os = s.os;
    S.early = new Map((s.early || []).map(d => [d.id, d]));
    S.plan = s.plan; S.polite = s.polite; S.net = s.net || null; S.power = s.power; S.profile = s.profile; S.quota = s.quota;
    if (s.alert && !S.alertShown) { S.alertShown = true; toast(s.alert, { ms: 12000, kind: "error" }); api("alert/seen", {}).catch(() => {}); }
    if (S.filter.startsWith("q:") && !S.queues.some(q => "q:" + q.id === S.filter)) S.filter = "all";
    for (const id of [...S.selected]) if (!S.byId.has(id)) S.selected.delete(id);
    listState();
    renderAll();
    for (const id of S.progress.keys()) updateProgress(id);
    $$(".dlg").forEach(d => d._draw && d._draw());
    powerUI();
    // notices
    const maxSeq = s.notices.reduce((a, n) => Math.max(a, n.seq), 0);
    if (S.lastSeq === null) S.lastSeq = maxSeq;
    for (const n of s.notices) {
      if (n.seq <= S.lastSeq) continue;
      if (n.kind === "complete") {
        const dl = S.progress.get(n.id); if (dl) dl.close();
        downloadFinished(n.id);
        if (S.settings?.showCompleteDialog ?? true) completeDialog(n.id);
      } else if (n.kind === "queueDone" || n.kind === "syncDone" || n.kind === "grabDone") toast(n.text || t("n.queueDone"), 4000);
      else if (n.kind === "error") {
        const d = S.byId.get(n.id);
        if (d) {
          const args = { name: nm(dname(d)), error: d.error };
          const msg = d.link ? th("n.nameError", args) : d.tryAgain ? th("n.couldntRetry", { ...args, wait: fmtWait((d.tryAgain - Date.now()) / 1000) }) : th("n.couldnt", args);
          if (!S.progress.has(n.id)) toast(msg, { ms: d.link ? 8000 : 5000, kind: "error", action: { label: t("btn.show"), fn: () => showDownload(n.id) } });
          else { note(msg, "error"); say(msg, true); } // (its window shows the error already; this is for the ears and the list)
        }
      }
      else if (n.kind === "noShare") {
        const d = S.byId.get(n.id), why = d && d.torrent && d.torrent.noShare;
        toast(why ? th("sh.toast", { name: nm(dname(d)), why: th("sh.no." + why.why, { file: nm(why.file || "") }) }) : n.text, { ms: 8000, kind: "warn" });
      }
      else if (n.kind === "threat") toast(t("n.virus", { text: n.text }), { ms: 8000, kind: "error" });
      else if (n.kind === "sound") playChime(n.text || n.id);
      else if (n.kind === "planStart" || n.kind === "planEnd" || n.kind === "metered" || n.kind === "sched" || n.kind === "profile") toast(n.text, 6000);
      else if (n.kind === "quotaWarn" || n.kind === "refused") toast(n.text, { ms: 8000, kind: "warn" });
      else if (n.kind === "quotaStop") { quotaDialog(); const m = t("n.quotaUsed"); note(m, "warn"); say(m, true); }
      else if (n.kind === "linkRenewed" || n.kind === "checkOk" || n.kind === "phonePaired") toast(n.text, 4000);
      else if (n.kind === "checkBad" || n.kind === "checkErr") toast(n.text, { ms: 8000, kind: "error" });
    }
    S.lastSeq = Math.max(S.lastSeq, maxSeq);
    processPrompts(s.prompts);
  };
  es.onerror = () => {
    clearTimeout(lostTimer);
    lostTimer = setTimeout(() => { if (!$("#lost")) toast(t("n.lost"), { id: "lost", sticky: true, kind: "error" }); }, 2500);
  };
}

// ------------------------------------------------------------------ keyboard, paste, drop
document.addEventListener("keydown", e => {
  if (e.key === "Escape") {
    if ($("#ctx").style.display === "block" || $(".mi.open")) { closeMenus(); return; }
    const top = $$(".dlg").sort((a, b) => (+b.style.zIndex) - (+a.style.zIndex))[0];
    if (top) { top.close(); return; }
  }
  if (e.key === "F10" && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) { if (!e.target.closest(".dlg,dialog")) { e.preventDefault(); toggleMenubar(); } return; }
  // shortcuts work anywhere but in text fields and dialogs; the list's own keys when it has the
  // focus (or nothing has). Menus, the sidebar and the column headers take their keys first.
  if (e.target.closest("input,textarea,select,.dlg,dialog")) return;
  const k = e.key.toLowerCase(); const c = e.ctrlKey || e.metaKey;
  if (c && k === "n") { e.preventDefault(); addUrlDialog(); }
  else if (c && k === "b") { e.preventDefault(); batchDialog(); }
  else if (c && k === "g") { e.preventDefault(); grabberDialog(); }
  else if (c && k === "o") { e.preventDefault(); openTorrentFile(); }
  else if (c && k === "a") { e.preventDefault(); visible().forEach(d => S.selected.add(d.id)); renderTable(); updateToolbar(); }
  else if (c && k === "f") { e.preventDefault(); $("#search").focus(); }
  else if (k === "enter" && e.altKey) { e.preventDefault(); propsDialog(sel()[0]); }
  else if (e.target === $("#tbl") || e.target === document.body) listKey(e);
});

// ---- the list from the keyboard (the WAI-ARIA grid pattern)
// One Tab stop; the focused row is the grid's active descendant, so it survives rows being reused
// as the list scrolls. Up/Down move (Shift extends the selection, Ctrl moves without selecting,
// Ctrl+Space then toggles a row), Page Up/Down, Home/End; Up from the first row goes to the column
// headers. Enter, Space, Delete and Shift+Delete act on the selection as before; Shift+F10 or the
// Menu key opens the context menu at the focused row.
let lastKb = false; // the last thing used was the keyboard (focus rings show then)
function listKey(e) {
  const k = e.key, c = e.ctrlKey || e.metaKey;
  if (gridKey(e)) e.preventDefault();
  else if (k === "Delete") { e.preventDefault(); removeSel(e.shiftKey); }
  else if (k === "Enter") { const sl = sel(); (sl.length ? sl : [S.byId.get(LIST.focus)].filter(Boolean)).forEach(d => rowOpen(d.id)); }
  else if (k === " " && c) { e.preventDefault(); const id = LIST.focus; if (id) { S.selected.has(id) ? S.selected.delete(id) : S.selected.add(id); S.anchor = id; renderTable(); updateToolbar(); } }
  else if (k === " ") { e.preventDefault(); const sl = sel(); if (sl.some(d => ["downloading", "connecting", "queued"].includes(d.status))) act("pause"); else act("start"); }
  else if (k === "ContextMenu" || (k === "F10" && e.shiftKey)) { e.preventDefault(); rowMenuKey(); }
}
function gridKey(e) {
  const k = e.key, c = e.ctrlKey || e.metaKey, tbl = $("#tbl");
  if (!["ArrowDown", "ArrowUp", "PageDown", "PageUp", "Home", "End"].includes(k) || e.altKey) return false;
  const ids = listIds(), n = ids.length, i = LIST.focus ? idx(LIST.focus) : -1, at = Math.max(i, 0);
  if (document.activeElement !== tbl) tbl.focus({ preventScroll: true });
  if (!n) return true;
  if (k === "ArrowUp" && i === 0 && !e.shiftKey && !c) { focusHead(); return true; }
  const page = Math.max(1, Math.floor(($("#tablewrap").clientHeight - $("#thead").offsetHeight) / LIST.rowH) - 1);
  const to = { ArrowDown: i < 0 ? 0 : i + 1, ArrowUp: at - 1, PageDown: at + page, PageUp: at - page, Home: 0, End: n - 1 }[k];
  moveFocus(Math.max(0, Math.min(n - 1, to)), e.shiftKey ? "extend" : c ? "move" : "select");
  return true;
}
// moveFocus moves the keyboard's row: selecting just it, extending the selection from the anchor
// to it, or leaving the selection be.
function moveFocus(i, how) {
  const ids = listIds(), id = ids[i];
  if (how === "select") { S.selected = new Set([id]); S.anchor = id; }
  else if (how === "extend") {
    let a = S.anchor ? idx(S.anchor) : -1;
    if (a < 0) { a = LIST.focus && idx(LIST.focus) >= 0 ? idx(LIST.focus) : i; S.anchor = ids[a]; }
    S.selected = new Set(ids.slice(Math.min(a, i), Math.max(a, i) + 1));
  }
  LIST.focus = id;
  showRow(i); renderTable(); updateToolbar();
}
// showRow scrolls the list just enough to show row i whole, below the sticky header.
function showRow(i) {
  if (i < 0) return;
  const w = $("#tablewrap"), H = LIST.rowH, view = w.clientHeight - $("#thead").offsetHeight;
  if (i * H < w.scrollTop) w.scrollTop = i * H;
  else if ((i + 1) * H > w.scrollTop + view) w.scrollTop = Math.ceil((i + 1) * H - view);
  LIST.scrollTop = w.scrollTop;
}
function focusHead() { ($("#thead th[tabindex='0']") || $("#thead th"))?.focus(); }
// rowMenuKey opens the rows' context menu from the keyboard, at the focused row (selecting it
// first unless it's part of the selection).
function rowMenuKey() {
  const ids = listIds(), tbl = $("#tbl");
  if (!ids.length) return;
  if (!LIST.focus || idx(LIST.focus) < 0) LIST.focus = ids[0];
  const id = LIST.focus;
  if (!S.selected.has(id)) { S.selected = new Set([id]); S.anchor = id; }
  if (document.activeElement !== tbl) tbl.focus({ preventScroll: true });
  showRow(idx(id)); renderTable(); updateToolbar();
  const r = (LIST.rows.get(id) || $("#tablewrap")).getBoundingClientRect();
  rowCtx({ preventDefault() {}, clientX: r.left + 24, clientY: r.bottom });
}
(() => {
  const tbl = $("#tbl");
  tbl.setAttribute("role", "grid"); tbl.tabIndex = 0;
  tbl.setAttribute("aria-label", t("menu.downloads")); tbl.setAttribute("aria-multiselectable", "true");
  $("#thead").setAttribute("aria-rowindex", "1");
  tbl.addEventListener("focus", () => { // (Tab in: the anchor, else the first selected or first row)
    const ids = listIds();
    if (!LIST.focus || idx(LIST.focus) < 0) LIST.focus = [S.anchor, ...S.selected].find(id => id && idx(id) >= 0) || ids[0] || null;
    if (lastKb && LIST.focus) showRow(idx(LIST.focus));
    renderRows();
  });
  // a context menu the system raised from the keyboard lands on the grid itself
  tbl.addEventListener("contextmenu", e => { if (e.target === tbl) { e.preventDefault(); if ($("#ctx").style.display !== "block") rowMenuKey(); } });
  // column headers: Left/Right/Home/End between them, Enter or Space sorts, Down back to the rows,
  // Shift+F10 or the Menu key for the columns menu
  $("#thead").addEventListener("keydown", e => {
    const th = e.target.closest("th"); if (!th || e.altKey) return;
    const ths = $$("#thead th"), i = ths.indexOf(th), k = e.key;
    const to = j => { const t = ths[Math.max(0, Math.min(ths.length - 1, j))]; ths.forEach(x => x.tabIndex = x === t ? 0 : -1); LIST.head = t.dataset.k; t.focus(); };
    if (k === "ArrowLeft" || k === "ArrowRight") to(i + (k === "ArrowLeft" ? -1 : 1));
    else if (k === "Home" || k === "End") to(k === "Home" ? 0 : ths.length - 1);
    else if (k === "Enter" || k === " ") sortBy(th.dataset.k);
    else if (k === "ArrowDown") tbl.focus();
    else if (k === "ContextMenu" || (k === "F10" && e.shiftKey)) { const r = th.getBoundingClientRect(); showMenuAt({ getBoundingClientRect: () => ({ left: r.left, top: r.top, bottom: r.bottom }) }, columnMenuItems()); }
    else return;
    e.preventDefault(); e.stopPropagation();
  });
  // Alt pressed and let go on its own (or F10) goes to the menu bar and back
  let altAlone = false;
  document.addEventListener("keydown", e => {
    altAlone = e.key === "Alt" ? !e.repeat || altAlone : false;
    if (!["Shift", "Control", "Alt", "Meta"].includes(e.key)) { lastKb = true; document.documentElement.classList.add("kbd"); }
  }, true);
  document.addEventListener("keyup", e => {
    if (e.key !== "Alt" || !altAlone) return;
    altAlone = false;
    if (!e.target.closest(".dlg,dialog")) { e.preventDefault(); toggleMenubar(); }
  });
  addEventListener("blur", () => altAlone = false);
  document.addEventListener("mousedown", () => { altAlone = lastKb = false; document.documentElement.classList.remove("kbd"); }, true);
})();
document.addEventListener("paste", e => {
  if (e.target.closest("input,textarea")) return;
  const t = (e.clipboardData.getData("text") || "").trim();
  if (LINK_RE.test(t)) addUrlDialog(t);
});
document.addEventListener("dragover", e => e.preventDefault());
document.addEventListener("drop", e => {
  if (e.target.closest("dialog.dlg input, dialog.dlg textarea")) return; // dropped text goes into the field it landed on
  e.preventDefault();
  if (modals.length) return; // a modal window is for one job: a drop doesn't open another window on top of it
  const tf = [...(e.dataTransfer.files || [])].filter(f => /\.torrent$/i.test(f.name));
  if (tf.length) return openTorrentFile(tf);
  const mg = (e.dataTransfer.getData("text/uri-list") || e.dataTransfer.getData("text") || "").split(/\s+/).filter(s => /^magnet:\?/i.test(s));
  if (mg.length) return mg.forEach(m => fileInfoDialog(m));
  const t = (e.dataTransfer.getData("text/uri-list") || e.dataTransfer.getData("text") || "").split(/\s+/).filter(s => URL_RE.test(s));
  if (t.length === 1) addUrlDialog(t[0]); else if (t.length > 1) { batchDialog(); $("#b-list").value = t.join("\n"); }
});
document.addEventListener("contextmenu", e => { if (!e.target.closest("tr,input,textarea")) e.preventDefault(); });

// ------------------------------------------------------------------ boot
$("#toolbar").setAttribute("aria-label", t("tb.commands"));
buildMenus(); buildToolbar(); buildHead();
// the status bar's buttons (it's redrawn often, so one listener for all)
$("#statusbar").addEventListener("click", e => {
  const b = e.target.closest("[data-sb]");
  if (b && b.dataset.sb === "profile") profileMenu(b); else if (b && b.dataset.sb === "quota") quotaDialog();
});
api("settings").then(s => {
  S.settings = s;
  let theme = ""; try { theme = localStorage.getItem("zl.theme") || ""; } catch {}
  if ((s.theme || "") !== theme) { s.theme = theme; api("theme", { theme }).catch(() => {}); } // (a theme chosen before the app kept it)
});
connect();
