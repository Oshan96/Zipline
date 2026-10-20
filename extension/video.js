// Zipline: shows a small "Download video" button over video players.
// Nothing is sent anywhere until the user clicks it.
(() => {
  const B = globalThis.browser ?? globalThis.chrome;
  if (window.__ziplineVideo) return;
  window.__ziplineVideo = true;

  const host = location.hostname.toLowerCase();
  const MIN_W = 200, MIN_H = 120;
  let enabled = false, current = null, hideTimer = null, rafId = 0, menuOpen = false;

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
  // "12:34 · up to 1080p · 340 MB" (the Add dialog in the app picks the quality)
  const metaOf = c => [c.live ? "Live" : fmtDur(c.duration), (c.quals || []).length > 1 ? `up to ${c.quals[0]}p` : (c.quals || []).length ? `${c.quals[0]}p` : "", fmtSize(c.size)].filter(Boolean).join(" · ");
  const msg = m => { try { return Promise.resolve(B.runtime.sendMessage(m)).catch(() => null); } catch { return Promise.resolve(null); } };

  // ---- UI (shadow DOM so page styles can't leak in or out). Built with DOM calls, never markup: pages
  // that enforce Trusted Types refuse markup from scripts, and nothing from the page can inject any.
  const el = (tag, cls, text) => { const n = document.createElement(tag); if (cls) n.className = cls; if (text != null) n.textContent = text; return n; };
  const CSS = `
  :host{all:initial}
  *{box-sizing:border-box;font:500 13px/1.3 "Segoe UI",system-ui,-apple-system,sans-serif}
  .btn{display:inline-flex;align-items:center;gap:7px;padding:7px 12px 7px 10px;border:0;border-radius:999px;cursor:pointer;max-width:min(340px,calc(100vw - 16px));
    background:#1a6cf0;color:#fff;box-shadow:0 4px 14px rgba(0,0,0,.35);white-space:nowrap;transition:background .15s,opacity .15s}
  .lbl{overflow:hidden;text-overflow:ellipsis}
  .btn:hover{background:#155fd6}
  .btn:focus-visible{outline:2px solid #fff;outline-offset:2px}
  .btn.ok{background:#1f9d55}.btn.bad{background:#d93a3a}
  .btn svg{flex:none}
  .menu{position:absolute;right:0;top:calc(100% + 6px);width:max-content;min-width:min(260px,calc(100vw - 16px));max-width:min(380px,calc(100vw - 16px));max-height:calc(100vh - 80px);overflow:auto;padding:6px;border-radius:10px;
    background:#fff;color:#1c2330;box-shadow:0 10px 34px rgba(0,0,0,.35);display:none}
  .menu.open{display:block}
  .h{padding:6px 10px 4px;color:#6a7486;font-size:12px}
  .it{display:flex;gap:10px;align-items:center;width:100%;padding:8px 10px;border:0;border-radius:7px;background:none;color:inherit;cursor:pointer;text-align:left}
  .it:hover,.it:focus-visible{background:#e8f0ff;outline:none}
  .it .t{flex:1;min-width:0;display:flex;flex-direction:column;gap:1px}
  .it .n{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .it .m{color:#6a7486;font-size:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .tag{flex:none;min-width:40px;text-align:center;font-size:10.5px;padding:1px 6px;border-radius:4px;background:#eef1f6;color:#5b6475}
  .foot{display:flex;justify-content:flex-end;border-top:1px solid #e9edf2;margin-top:4px;padding:6px 4px 0}
  .link{border:0;background:none;color:#6a7486;font-size:12px;cursor:pointer;padding:4px 6px;border-radius:5px}
  .link:hover{color:#1c2330;background:#f3f5f8}
  @media (prefers-color-scheme: dark){
    .menu{background:#1d2128;color:#e5e9f0}
    .h,.it .m,.link{color:#8d97a8}
    .it:hover,.it:focus-visible{background:#24375a}
    .tag{background:#2b313b;color:#aab3c2}
    .foot{border-color:#2f3540}
    .link:hover{color:#e5e9f0;background:#252b35}
  }
`;
  const root = document.createElement("zipline-video-button");
  root.style.cssText = "position:fixed;z-index:2147483646;top:0;left:0;display:none;";
  const sh = root.attachShadow({ mode: "open" });
  const btn = el("button", "btn"), lbl = el("span", "lbl", "Download video"), menu = el("div", "menu"), box = el("div");
  btn.setAttribute("part", "btn");
  btn.setAttribute("aria-haspopup", "menu");
  menu.setAttribute("role", "menu");
  box.style.position = "relative";
  const svg = (tag, attrs) => { const n = document.createElementNS("http://www.w3.org/2000/svg", tag); for (const k in attrs) n.setAttribute(k, attrs[k]); return n; };
  const icon = svg("svg", { width: 16, height: 16, viewBox: "0 0 16 16", fill: "none", stroke: "currentColor", "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round" });
  icon.append(svg("path", { d: "M8 2.5v8M4.5 7.5 8 11l3.5-3.5M3 13.5h10" }));
  btn.append(icon, lbl);
  box.append(btn, menu);
  sh.append(el("style", "", CSS), box);

  function place() {
    if (!current || !current.isConnected) return hide(true);
    const r = current.getBoundingClientRect();
    if (r.width < MIN_W || r.height < MIN_H || r.bottom < 0 || r.top > innerHeight) { root.style.display = "none"; return; }
    root.style.display = "block";
    const bw = root.offsetWidth || 150;
    root.style.left = Math.max(4, Math.min(r.right - bw - 10, innerWidth - bw - 4)) + "px";
    root.style.top = Math.max(4, r.top + 10) + "px";
    rafId = requestAnimationFrame(place);
  }
  function show(v) {
    if (!enabled) return;
    clearTimeout(hideTimer);
    if (current !== v) { current = v; closeMenu(); setLabel("Download video"); }
    cancelAnimationFrame(rafId); place();
  }
  function hide(now) {
    clearTimeout(hideTimer);
    const go = () => { if (menuOpen) return; cancelAnimationFrame(rafId); root.style.display = "none"; current = null; };
    now ? go() : (hideTimer = setTimeout(go, 1500));
  }
  function setLabel(t, cls = "") { lbl.textContent = t; btn.title = t.length > 30 ? t : ""; btn.className = "btn " + cls; }
  function closeMenu() { menuOpen = false; menu.classList.remove("open"); }
  // keep the menu on screen: it hangs from the button's right edge, so a button near the left edge pushes it out
  function fitMenu() {
    menu.style.right = "0"; menu.style.left = "auto";
    const r = menu.getBoundingClientRect();
    if (r.left < 4) { menu.style.right = "auto"; menu.style.left = (4 - root.getBoundingClientRect().left) + "px"; }
  }

  root.addEventListener("mouseenter", () => clearTimeout(hideTimer));
  root.addEventListener("mouseleave", () => hide());

  // One entry per video: a stream is one entry with all its qualities (the app's Add dialog picks one).
  async function candidates() {
    const src = current && (current.currentSrc || current.src);
    const r = await msg({ type: "media-list", src });
    return (r && r.items) || [];
  }

  async function sendOne(url) {
    closeMenu(); setLabel("Sending…");
    const r = await msg({ type: "send-media", url, frameUrl: location.href });
    if (r && r.ok) setLabel("Sent to Zipline ✓", "ok");
    else setLabel(r && r.error ? r.error : "Couldn't reach Zipline", "bad");
    setTimeout(() => { setLabel("Download video"); }, 2600);
  }

  btn.addEventListener("click", async e => {
    e.preventDefault(); e.stopPropagation();
    if (menuOpen) return closeMenu();
    const list = await candidates();
    if (!list.length) {
      setLabel("Play the video first", "bad");
      setTimeout(() => setLabel("Download video"), 2400);
      return;
    }
    if (list.length === 1) return sendOne(list[0].url);
    menu.replaceChildren(el("div", "h", "Choose what to download"));
    for (const c of list) {
      const b = el("button", "it"), t = el("span", "t"), meta = metaOf(c);
      b.setAttribute("role", "menuitem");
      b.title = c.url;
      t.append(el("span", "n", c.title));
      if (meta) t.append(el("span", "m", meta));
      b.append(el("span", "tag", c.kind === "hls" ? "HLS" : c.kind === "dash" ? "DASH" : c.kind === "audio" ? "Audio" : "File"), t);
      b.onclick = ev => { ev.stopPropagation(); sendOne(c.url); };
      menu.append(b);
    }
    const foot = el("div", "foot"), hideBtn = el("button", "link", "Don't show on this site");
    hideBtn.dataset.hide = "";
    foot.append(hideBtn);
    menu.append(foot);
    hideBtn.onclick = ev => {
      ev.stopPropagation();
      msg({ type: "hide-button", host }); // (kept in Zipline's Options → Browser)
      enabled = false; closeMenu(); hide(true);
    };
    menuOpen = true; menu.classList.add("open");
    fitMenu();
    menu.querySelector(".it")?.focus();
  });
  menu.addEventListener("keydown", e => { if (e.key === "Escape") { closeMenu(); btn.focus(); } });
  document.addEventListener("mousedown", e => { if (menuOpen && !e.composedPath().includes(root)) { closeMenu(); hide(); } }, true);

  // ---- watch videos
  // Only videos on screen are looked at (an IntersectionObserver keeps that set), and new ones are found
  // in what pages add (a MutationObserver that looks at added nodes only, a moment later), in open
  // shadow roots too: those of added elements, and any the pointer passes through.
  const bound = new WeakSet(), roots = new WeakSet(), onScreen = new Set();
  const io = new IntersectionObserver(es => { for (const e of es) e.isIntersecting ? onScreen.add(e.target) : onScreen.delete(e.target); });
  function bind(v) {
    if (bound.has(v)) return;
    bound.add(v);
    io.observe(v);
    v.addEventListener("mouseenter", () => show(v));
    v.addEventListener("mousemove", () => show(v));
    v.addEventListener("mouseleave", e => { if (!e.relatedTarget || !root.contains(e.relatedTarget)) hide(); });
    v.addEventListener("play", () => { show(v); hide(); });
  }
  // scan: the videos in an added element (or shadow root), and the open shadow roots inside it
  function scan(node) {
    if (node.nodeName === "VIDEO") bind(node);
    if (!node.querySelectorAll) return;
    node.querySelectorAll("video").forEach(bind);
    if (node.shadowRoot) watchRoot(node.shadowRoot);
    const w = document.createTreeWalker(node, NodeFilter.SHOW_ELEMENT);
    for (let n = w.nextNode(); n; n = w.nextNode()) if (n.shadowRoot) watchRoot(n.shadowRoot);
  }
  let added = [], soon = false, everything = false;
  const later = window.requestIdleCallback ? f => requestIdleCallback(f, { timeout: 400 }) : f => setTimeout(f, 150);
  const mo = new MutationObserver(recs => {
    for (const r of recs) for (const n of r.addedNodes) if (n.nodeType === 1) added.push(n);
    if (added.length > 3000) { added = []; everything = true; } // (a whole new page: look at it once instead)
    if (!soon && (added.length || everything)) { soon = true; later(drain); }
  });
  function drain() {
    soon = false;
    const list = everything ? [document.documentElement] : added;
    added = []; everything = false;
    for (const n of list) if (n.isConnected) scan(n);
  }
  function watchRoot(r) {
    if (roots.has(r) || r === sh) return; // (not the button's own)
    roots.add(r);
    mo.observe(r, { childList: true, subtree: true });
    scan(r);
  }
  // shadow roots made after their element was added: found when the pointer passes through them
  document.addEventListener("mouseover", e => {
    if (!enabled) return;
    for (const n of e.composedPath()) if (n instanceof ShadowRoot && !roots.has(n)) watchRoot(n);
  }, { passive: true, capture: true });
  // players often cover the <video> with their own controls layer, so also follow the pointer
  // (once a frame, over the videos on screen only)
  let px = 0, py = 0, aimed = false;
  document.addEventListener("mousemove", e => {
    if (!enabled || !onScreen.size) return;
    px = e.clientX; py = e.clientY;
    if (!aimed) { aimed = true; requestAnimationFrame(aim); }
  }, { passive: true });
  function aim() {
    aimed = false;
    for (const v of onScreen) {
      if (!v.isConnected) { onScreen.delete(v); continue; }
      const r = v.getBoundingClientRect();
      if (r.width >= MIN_W && r.height >= MIN_H && px >= r.left && px <= r.right && py >= r.top && py <= r.bottom) { show(v); return; }
    }
    if (current && !menuOpen) hide();
  }

  const prefsOn = p => !!p && p.videoButton !== false && !(p.noButton || []).includes(host);
  (async () => {
    const r = await msg({ type: "is-blocked", host });
    if (r && r.blocked) return;
    const s = await B.storage.local.get({ prefs: null });
    enabled = prefsOn(s.prefs || { videoButton: true });
    (document.body || document.documentElement).appendChild(root);
    mo.observe(document.documentElement, { childList: true, subtree: true });
    scan(document.documentElement);
  })();

  B.storage.onChanged.addListener(ch => {
    if (ch.prefs) {
      enabled = prefsOn(ch.prefs.newValue || { videoButton: true });
      if (!enabled) hide(true);
    }
  });
})();
