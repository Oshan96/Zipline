// Works in Chrome, Edge, Brave, Opera (chrome.*) and Firefox (browser.*).
const B = globalThis.browser ?? globalThis.chrome;
const $ = id => document.getElementById(id);
const send = m => Promise.resolve(B.runtime.sendMessage(m)).catch(() => null);
const fmtSize = b => {
  if (!(b > 0)) return "";
  const u = ["B", "KB", "MB", "GB"]; let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i ? b.toFixed(b < 10 ? 1 : 0) : b) + " " + u[i];
};

const fmtDur = s => {
  s = Math.round(s);
  if (!(s > 0)) return "";
  const h = Math.floor(s / 3600), m = String(Math.floor(s % 3600 / 60)).padStart(h ? 2 : 1, "0"), x = String(s % 60).padStart(2, "0");
  return h ? `${h}:${m}:${x}` : `${m}:${x}`;
};
// The app's theme: the last one known at once (no flash), then what the app says now.
const useTheme = th => {
  if (th === "light" || th === "dark") document.documentElement.dataset.theme = th; else delete document.documentElement.dataset.theme;
  try { localStorage.setItem("zl.theme", th || ""); } catch {}
};
try { useTheme(localStorage.getItem("zl.theme")); } catch {}
const el = (tag, cls, text) => { const n = document.createElement(tag); if (cls) n.className = cls; if (text != null) n.textContent = text; return n; };

// "12:34 · up to 1080p · 340 MB" (the Add dialog in the app picks the quality)
const metaOf = it => [it.live ? "Live" : fmtDur(it.duration), (it.quals || []).length > 1 ? `up to ${it.quals[0]}p` : (it.quals || []).length ? `${it.quals[0]}p` : "", fmtSize(it.size)].filter(Boolean).join(" · ");

(async () => {
  const [tab] = await B.tabs.query({ active: true, currentWindow: true });
  let host = "";
  try { host = new URL(tab?.url || "").hostname.toLowerCase(); } catch {}

  const p = await send({ type: "ping" }), up = !!(p && p.ok);
  showLink(p); // connected, waiting to be allowed, or which side needs updating (linkpopup.js)
  $("open").disabled = $("opts").disabled = !up;
  $("open").onclick = async () => { if ((await send({ type: "show-app" }) || {}).ok) window.close(); };
  $("opts").onclick = async () => { if ((await send({ type: "show-app", page: "browser" }) || {}).ok) window.close(); };

  // videos found on this tab
  const box = $("videos");
  const bl = host ? await send({ type: "is-blocked", host }) : null;
  const blocked = bl && bl.blocked;
  const r = tab && !blocked ? await send({ type: "media-list", tabId: tab.id }) : null;
  const items = (r && r.items) || [];
  $("vcount").textContent = items.length ? String(items.length) : "";
  if (blocked) box.replaceChildren(el("div", "empty", (bl.message || "Zipline doesn't download from this site") + "."));
  else if (!items.length) box.replaceChildren(el("div", "empty", "No videos found yet. Start playing the video, then open this again."));
  else {
    box.replaceChildren();
    for (const it of items) { // (newest first; a stream is one entry, however many qualities it has)
      const row = el("div", "row"), t = el("span", "t"), meta = el("span", "m", metaOf(it)), b = el("button", "sm", "Download");
      row.title = it.url;
      t.append(el("span", "n", it.title), meta);
      row.append(el("span", "tag", it.kind === "hls" ? "HLS" : it.kind === "dash" ? "DASH" : it.kind === "audio" ? "Audio" : "File"), t, b);
      b.onclick = async () => {
        b.textContent = "…";
        const res = await send({ type: "send-media", tabId: tab.id, url: it.url, frameUrl: tab.url });
        b.textContent = res && res.ok ? "Sent ✓" : "Failed";
        b.className = "sm " + (res && res.ok ? "done" : "bad");
        meta.className = "m" + (res && res.ok ? "" : " bad");
        meta.textContent = res && res.ok ? metaOf(it) : (res && res.error) || "Couldn't reach Zipline";
        meta.hidden = !meta.textContent;
      };
      meta.hidden = !meta.textContent;
      box.appendChild(row);
    }
  }

  // download all links on the page
  $("all").disabled = !/^https?:/i.test(tab?.url || "");
  $("all").onclick = async () => {
    $("all").disabled = true; $("allmsg").textContent = "Collecting links…";
    const r = await send({ type: "page-links", tabId: tab.id });
    if (r && r.ok) { $("allmsg").textContent = `Sent ${r.count} links to Zipline ✓`; setTimeout(() => window.close(), 900); }
    else { $("allmsg").textContent = (r && r.error) || "Couldn't reach Zipline"; $("all").disabled = false; }
  };

  // scan the whole site: Zipline's window opens the site grabber on this page, signed in as the browser is
  $("scan").disabled = !/^https?:/i.test(tab?.url || "");
  $("scan").onclick = async () => {
    $("scan").disabled = true; $("scanmsg").textContent = "Opening Zipline…";
    const r = await send({ type: "scan-site", tabId: tab.id });
    if (r && r.ok) { $("scanmsg").textContent = "Zipline is ready to scan this site ✓"; setTimeout(() => window.close(), 900); }
    else { $("scanmsg").textContent = (r && r.error) || "Couldn't reach Zipline"; $("scan").disabled = false; }
  };

  // Firefox grants site access separately; ask once from here (needs a click).
  if (B.permissions && B.permissions.contains) {
    const all = { origins: ["<all_urls>"] };
    const ok = await B.permissions.contains(all).catch(() => true);
    $("perm").hidden = ok;
    $("grant").onclick = async () => { if (await B.permissions.request(all).catch(() => false)) $("perm").hidden = true; };
  }

  // settings: kept in the app (Options → Browser); these are quick switches for two of them
  const pr = (await send({ type: "prefs" })) || {};
  let s = pr.prefs || { takeover: false, minKB: 256, videoButton: true, noButton: [] };
  if (pr.theme !== undefined) useTheme(pr.theme);
  const show = () => {
    $("takeover").checked = !!s.takeover;
    $("vbtn").checked = s.videoButton !== false;
    $("siteoff").hidden = !(host && (s.noButton || []).includes(host));
    $("tkhint").textContent = s.takeover
      ? `${s.minKB > 0 ? `Downloads of ${fmtSize(s.minKB * 1024)} or more` : "Downloads"} go to Zipline as the browser asked for them. Close Zipline's Add dialog to give one back to the browser.`
      : "Off by default. Sizes, file types and sites to leave alone are in Zipline's browser options.";
  };
  show();
  const change = async c => {
    const r = await send({ type: "set-prefs", change: c });
    if (r && r.prefs) s = r.prefs;
    show();
    $("setmsg").textContent = r && r.sent ? "" : "Saved here. Zipline gets the change when it's running.";
  };
  $("takeover").onchange = () => change({ takeover: $("takeover").checked });
  $("vbtn").onchange = () => change({ videoButton: $("vbtn").checked });
  $("siteon").onclick = () => change({ noButton: (s.noButton || []).filter(h => h !== host) });

  // keyboard shortcuts (the browser lets you change them)
  const keys = B.commands && B.commands.getAll ? await B.commands.getAll().catch(() => []) : [];
  const said = { "zl-download": "Download the link under the pointer, or this page's video", "_execute_action": "Open this menu" };
  for (const c of keys.filter(c => said[c.name]).sort((a, b) => Object.keys(said).indexOf(a.name) - Object.keys(said).indexOf(b.name))) {
    $("keys").append(c.shortcut ? el("kbd", "", c.shortcut) : el("span", "unset", "Not set"), el("span", "", said[c.name]));
  }
  const setKeys = B.commands && B.commands.openShortcutSettings ? () => B.commands.openShortcutSettings()
    : typeof B.runtime.getBrowserInfo !== "function" ? () => B.tabs.create({ url: "chrome://extensions/shortcuts" }) : null;
  if ($("keys").childElementCount && setKeys) {
    const a = el("a", "", "Change shortcuts");
    a.onclick = () => { setKeys(); window.close(); };
    $("keys").append(el("span"), a);
  }

  // add a link by hand
  const add = async () => {
    const url = $("url").value.trim();
    if (!/^(https?|ftps?):\/\//i.test(url)) { $("msg").textContent = "Enter an http, https or ftp link"; return; }
    const res = await send({ type: "send", url, referer: tab?.url || "", title: tab?.title || "", tabId: tab?.id });
    $("msg").textContent = res && res.ok ? "Sent to Zipline ✓" : (res && res.error) || "Couldn't reach Zipline";
    if (res && res.ok) $("url").value = "";
  };
  $("go").onclick = add;
  $("url").addEventListener("keydown", e => { if (e.key === "Enter") add(); });
})();
