"use strict";
/* Zipline — the window's side of phone control (phone.go, phonenet.go, phonepair.go): the question "A phone
   wants to connect", and the parts of Options → Phone that say which networks the phone page is on and which
   phones are waiting for an answer. Loaded after app.js and extlink.js, whose helpers it uses (api, dialog,
   toast, $, $$, h, esc, the i18n functions). */

// phoneDialog asks whether a phone that opened a pairing link may connect. It looks like pairDialog (the
// browser extension's question). Closing it leaves the request unanswered (Options → Phone still shows it,
// and it runs out after two minutes); the window starts on Deny so a stray Enter can't allow it.
function phoneDialog(p) {
  return new Promise(done => {
    const answer = async (el, allow) => {
      const r = await api("phone", { action: "answer", token: p.jobId, allow });
      if (r.error) throw new Error(r.error);
      el.close();
      if (allow) toast(th("phq.allowed", { name: nm(p.title) }), 4000);
    };
    dialog({ title: t("phq.title"), width: 480, alert: true, focus: "#phq-deny", onClose: done,
      body: `<p style="margin:0 0 12px">${te("phq.body")}</p>
        <div class="kv" style="margin-bottom:12px">
          <div>${te("phq.phoneL")}</div><div class="wrap"><bdi>${esc(p.title)}</bdi></div>
          <div>${te("phq.addrL")}</div><div class="wrap"><code style="word-break:break-all">${esc(p.name)}</code></div>
        </div>
        <div class="hint" style="margin:10px 0 0">${te("phq.warn")}</div>`,
      buttons: [
        { label: t("pair.allow"), primary: true, busy: t("btn.working"), fn: el => answer(el, true) },
        { label: t("pair.deny"), id: "phq-deny", busy: t("btn.working"), fn: el => answer(el, false) },
      ] });
  });
}

// phoneState is the line under the phone page's port: where it can be reached, or why it can't.
function phoneState(v) {
  const addrs = v.addrs || [], nets = v.nets || [];
  if (v.error) return { tone: " bad", text: v.error };
  if (v.running && addrs.length) return { tone: " good", text: t("o.phReady", { addr: addrs[0], port: String(v.port) }) };
  if (!v.on) return { tone: "", text: "" };
  const off = nets.filter(n => n.here && n.public && !n.allowed).length; // marked Public here, and not allowed
  if (off) return { tone: " bad", text: t("o.phNetPublic", { n: off }) };
  return { tone: nets.some(n => n.here) ? "" : " bad", text: t(nets.some(n => n.here) ? "o.phStarting" : "o.phNoNet") };
}

// phoneNets lists the networks the computer is on and whether the phone page is on there. One the system
// marks Public is off until it is allowed (here, for that network only); the choice is kept per network.
function phoneNets(box, v, reload) {
  const nets = v.nets || [];
  const rows = nets.map(n => {
    const st = !n.here ? "away" : n.public && !n.allowed ? "off" : n.allowed ? "allowed" : "on";
    const meta = [n.subnet, n.cat ? t("o.phCat." + n.cat) : "", t("o.phSt." + st)].filter(Boolean).join(" · ");
    const btn = n.public && !n.allowed ? `<button class="btn sm" data-allow="1">${te("o.phNetAllow")}</button>`
      : n.public ? `<button class="btn sm" data-allow="0">${te("o.phNetStop")}</button>` : "";
    return `<div class="dev-row ph-net" data-key="${esc(n.key)}"><div class="dev-name"><b><bdi>${esc(n.name)}</bdi></b><span class="hint" style="margin:0">${esc(meta)}</span></div>${btn}</div>`;
  }).join("");
  const html = !nets.length ? "" : `<div class="hint" style="margin:12px 0 6px">${te("o.phNetsHint")}</div><div class="dev-list">${rows}</div>${nets.some(n => n.public) ? `<div class="hint">${te("o.phNetPublicHint")}</div>` : ""}`;
  if (box._h === html) return;
  box.innerHTML = html; box._h = html;
  $$("[data-allow]", box).forEach(b => b.onclick = async () => {
    b.disabled = true;
    const r = await api("phone", { action: "allowNet", key: b.closest(".ph-net").dataset.key, allow: b.dataset.allow === "1" }).catch(e => ({ error: errText(e) }));
    if (r.error) toast(cap1(r.error), 5000);
    reload();
  });
}

// phoneAskRows are the phones waiting for an answer, first in the list of phones; wirePhoneAsks makes their buttons work.
function phoneAskRows(devs) {
  return devs.filter(d => d.ask).map(d => `<div class="dev-row ph-ask" data-token="${esc(d.ask.token)}"><div class="dev-name"><span class="ph-line">${ht("phq.line", { name: nm(d.ask.name), addr: nm(d.ask.addr) })}</span></div><div class="ph-btns"><button class="btn sm primary" data-yes="1">${te("pair.allow")}</button><button class="btn sm" data-yes="0">${te("pair.deny")}</button></div></div>`).join("");
}
function wirePhoneAsks(box, reload) {
  $$("[data-yes]", box).forEach(b => b.onclick = async () => {
    b.disabled = true;
    const r = await api("phone", { action: "answer", token: b.closest(".ph-ask").dataset.token, allow: b.dataset.yes === "1" }).catch(e => ({ error: errText(e) }));
    if (r.error) toast(cap1(r.error), 5000);
    reload();
  });
}

// phoneAsked tells the pairing box that a phone has opened the code and waits for the answer in the list below.
function phoneAsked(box, v, pairing) {
  const asked = (v.devices || []).some(d => d.id === pairing.id && d.ask);
  if (box._asked === asked) return;
  box._asked = asked;
  box.innerHTML = `<span class="spin"></span>${te(asked ? "o.phAsked" : "o.phWaiting")}`;
}
