"use strict";
/* Zipline — the window's side of the browser extension's link (extlink.go): letting a browser in over
   HTTP, how the extension is connected (Options → Browser), the note when Zipline couldn't have its
   usual port, and the question before the virus-scan program changes. Loaded after app.js, whose
   helpers it uses (api, dialog, confirmDialog, toast, $, h, esc, the i18n functions). */

// Zipline's own extension in Chrome and Edge (its id is fixed by manifest.json's key; nativehost.go has the same list)
const ZL_EXT_IDS = ["oogcbnoijbeekgjdkkjcbdbnhhjjbalf"];

// pairDialog asks whether a browser extension may connect over HTTP. Closing it leaves the request
// unanswered (it runs out after two minutes); the window starts on Deny so a stray Enter can't allow it.
function pairDialog(p) {
  return new Promise(done => {
    const origin = p.url || "", id = origin.replace(/^[a-z-]+:\/\//, "").replace(/\/$/, "");
    const firefox = origin.startsWith("moz-extension://"), known = !firefox && ZL_EXT_IDS.includes(id);
    const answer = async (el, allow) => {
      const r = await api("pairing", { id: p.jobId, allow });
      if (r.error) throw new Error(r.error);
      el.close();
      if (allow) toast(th("pair.allowed", { browser: nm(p.title || t("pair.aBrowser")) }), 4000);
    };
    dialog({ title: t("pair.title"), width: 480, alert: true, focus: "#pair-deny", onClose: done,
      body: `<p style="margin:0 0 12px">${te("pair.body")}</p>
        <div class="kv" style="margin-bottom:12px">
          <div>${te("pair.browserL")}</div><div>${esc(p.title || t("pair.unknown"))}</div>
          <div>${te("pair.versionL")}</div><div>${esc(p.name || t("pair.unknown"))}</div>
          <div>${te("pair.extL")}</div><div class="wrap"><code style="word-break:break-all">${esc(id || origin)}</code></div>
        </div>
        <div class="${known || firefox ? "info-box" : "err-box"}" style="margin-bottom:0"><div>${te(known ? "pair.known" : firefox ? "pair.firefox" : "pair.unknownId")}</div></div>
        <div class="hint" style="margin:10px 0 0">${ht("pair.warn")}</div>`,
      buttons: [
        { label: t("pair.allow"), primary: true, busy: t("btn.working"), fn: el => answer(el, true) },
        { label: t("pair.deny"), id: "pair-deny", busy: t("btn.working"), fn: el => answer(el, false) },
      ] });
  });
}

// extLinkPane fills the extension's part of Options → Browser: how it's connected, the versions, the
// port note and the browsers allowed over HTTP. It refreshes while that section is showing.
function extLinkPane(el) {
  const box = $("#o-link", el);
  if (!box) return;
  box.innerHTML = `<div class="dev-row lnk-row"><span class="lnk-dot" aria-hidden="true"></span><div class="dev-name"><span class="lnk-st"></span><span class="hint lnk-meta" style="margin:0"></span></div></div>
    <div class="info-box lnk-port" role="note" hidden><div></div></div>
    <div class="lnk-pairs" hidden><div class="hint" style="margin:12px 0 6px">${te("lnk.pairs")}</div><div class="dev-list"></div></div>`;
  const draw = v => {
    const st = v.extOld ? (v.state === "old" ? "old" : "extOld") : v.appOld ? "appOld" : v.state;
    const vars = { port: String(v.port || v.usual), min: v.minExt, ver: st === "appOld" ? v.app : v.ext.version };
    if (st === "appOld") vars.min = v.ext.minApp;
    $(".lnk-st", box).innerHTML = ht("lnk." + st, vars);
    $(".lnk-dot", box).className = "lnk-dot " + ({ native: "ok", http: "ok", unpaired: "warn", none: "" }[st] ?? "bad");
    $(".lnk-meta", box).textContent = v.ext.version
      ? t("lnk.meta", { ext: v.ext.version, app: v.app, seen: v.ext.seen ? fmtAgo(v.ext.seen) : "none" })
      : t("lnk.metaNoExt", { app: v.app, min: v.minExt });
    const port = $(".lnk-port", box);
    port.hidden = !(v.port && v.port !== v.usual);
    if (!port.hidden) $("div", port).textContent = t("lnk.port", { port: String(v.port), usual: String(v.usual) });
    const pairs = v.pairs || [], list = $(".lnk-pairs .dev-list", box);
    $(".lnk-pairs", box).hidden = !pairs.length;
    const html = pairs.map(p => `<div class="dev-row" data-id="${esc(p.id)}"><div class="dev-name"><b>${esc(p.browser || t("pair.aBrowser"))}</b><span class="hint" style="margin:0;overflow-wrap:anywhere">${te("lnk.pairMeta", { origin: (p.origin || "").replace(/^[a-z-]+:\/\//, ""), added: fmtDate(p.added), seen: p.seen ? fmtAgo(p.seen) : "none" })}</span></div><button class="btn sm" data-rm>${te("lnk.forget")}</button></div>`).join("");
    if (list._h !== html) {
      list.innerHTML = html; list._h = html;
      $$("[data-rm]", list).forEach(b => b.onclick = () => {
        const row = b.closest(".dev-row");
        confirmDialog(t("lnk.forgetTitle"), th("lnk.forgetBody", { name: nm($("b", row).textContent) }), t("lnk.forget"), async () => draw(await api("pairing", { remove: row.dataset.id }).then(() => api("linkstatus"))), { danger: true });
      });
    }
  };
  const load = async () => { try { draw(await api("linkstatus")); } catch {} };
  load();
  el._link = setInterval(() => { if (el.closed) clearInterval(el._link); else if ($(".pane[data-p=integration].active", el)) load(); }, 3000);
}

// confirmProgram asks before Zipline starts running a program on every finished download (Options →
// File checks → Scanner); true once the person agrees.
function confirmProgram(prog, args) {
  return new Promise(res => {
    const el = confirmDialog(t("scan.confirmTitle"), th("scan.confirmBody", { cmd: nm([prog, args].filter(Boolean).join(" ")) }), t("scan.confirmOk"), () => res(true), { danger: true });
    $$("code", el).forEach(c => c.style.wordBreak = "break-all"); // (a long path without spaces stays inside)
    const close = el.close;
    el.close = () => { close(); res(false); };
  });
}

// When another program had the port the extension knows, the window says so once.
api("linkstatus").then(v => { if (v.port && v.port !== v.usual) toast(t("lnk.port", { port: String(v.port), usual: String(v.usual) }), { ms: 12000, kind: "warn" }); }).catch(() => {});
