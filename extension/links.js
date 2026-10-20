// Zipline: hold a key while clicking a link to send it to Zipline (default Alt), or to let the
// browser download it even when Zipline takes over downloads (default Ctrl+Shift).
// The keys are set in the Zipline app: Options → Browser.
(() => {
  const B = globalThis.browser ?? globalThis.chrome;
  if (window.__ziplineKeys) return;
  window.__ziplineKeys = true;
  let keys = { zipline: "alt", browser: "ctrlshift" }, magnets = true;
  const load = () => B.storage.local.get({ appCfg: null }).then(s => {
    if (s.appCfg && s.appCfg.keys) keys = s.appCfg.keys;
    if (s.appCfg) magnets = s.appCfg.torrents !== false;
  }).catch(() => {});
  load();
  B.storage.onChanged.addListener(ch => { if (ch.appCfg) load(); });

  const combo = e => {
    if (e.metaKey) return "";
    if (e.ctrlKey && e.altKey && !e.shiftKey) return "ctrlalt";
    if (e.ctrlKey && e.shiftKey && !e.altKey) return "ctrlshift";
    if (e.shiftKey && e.altKey && !e.ctrlKey) return "shiftalt";
    if (e.altKey && !e.ctrlKey && !e.shiftKey) return "alt";
    return "";
  };
  const linkOf = e => {
    for (const n of e.composedPath()) {
      if ((n instanceof HTMLAnchorElement || n instanceof HTMLAreaElement) && n.href) return n;
    }
    return null;
  };

  // the link under the pointer, for the "Download with Zipline" shortcut (prefs.js reads it)
  document.addEventListener("mouseover", e => {
    const a = linkOf(e);
    globalThis.__ziplineHover = a && /^(https?|ftps?):/i.test(a.href) ? { url: a.href, page: location.href, t: Date.now() } : null;
  }, { passive: true, capture: true });
  document.addEventListener("mouseout", e => { if (!e.relatedTarget) globalThis.__ziplineHover = null; }, { passive: true, capture: true });

  // after the extension is updated or reloaded, this script keeps running in open tabs but can no
  // longer talk to it: then leave every click alone instead of swallowing it
  const alive = () => { try { return !!B.runtime?.id; } catch { return false; } };
  const send = msg => { try { return Promise.resolve(B.runtime.sendMessage(msg)); } catch (e) { return Promise.reject(e); } };
  document.addEventListener("click", e => {
    if (e.button !== 0 || !alive()) return;
    const c = combo(e);
    // magnet links open in Zipline (unless the app's Options → Torrents says otherwise, or you hold
    // the keep-in-browser keys); if Zipline isn't running, the link opens as usual
    const m = linkOf(e);
    if (m && /^magnet:/i.test(m.href) && (c === keys.zipline || (magnets && c !== keys.browser))) {
      e.preventDefault();
      e.stopImmediatePropagation();
      const href = m.href;
      send({ type: "magnet", url: href, referer: location.href })
        .then(r => { if (!r || !r.ok) location.href = href; }, () => { location.href = href; });
      return;
    }
    if (!c) return;
    const mode = c === keys.zipline ? "zipline" : c === keys.browser ? "browser" : "";
    if (!mode) return;
    const a = linkOf(e);
    if (!a || !/^(https?|ftps?):/i.test(a.href)) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    const href = a.href;
    if (mode === "zipline") {
      send({ type: "key-link", url: href, referer: location.href }).catch(() => { location.href = href; });
      return;
    }
    // open it as a plain click would, telling the extension to leave the download alone
    send({ type: "bypass", url: href }).catch(() => {}).then(() => {
      if (a.target && a.target !== "_self") window.open(href, a.target, "noopener");
      else location.assign(href);
    });
  }, true);
})();
