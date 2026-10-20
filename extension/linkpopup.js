// The popup's view of the link to the app (link.js keeps its state in storage.local "zlLink"): connected or not,
// a browser waiting to be allowed in Zipline's window, and which side needs updating when the versions don't fit.
// Its button asks link.js (through storage) to pair again or to start Zipline through the native host.
(() => {
  const LB = globalThis.browser ?? globalThis.chrome;
  const el = id => document.getElementById(id);
  let last = null, asked = "";

  function draw(p, s) {
    const up = p ? !!p.ok : !!s.up && (!!s.via || !s.pair);
    let st = up ? "Connected" : "App not running", dot = up ? "ok" : "bad", msg = "", btn = "", warn = false;
    if (s.need === "app") {
      st = "Update needed"; dot = "bad"; warn = true;
      msg = `This Zipline${s.app ? ` (${s.app})` : ""} is too old for the extension. Update Zipline to ${s.minApp} or later.`;
    } else if (s.need === "ext") {
      warn = true;
      msg = `Update the extension: Zipline ${s.app} works with the extension ${s.minExt} or later, and this is ${s.version}.`;
    } else if (!up && s.pair === "pending") {
      st = "Waiting for you"; dot = "";
      msg = "Zipline's window asks whether to let this browser in. Choose Allow there.";
    } else if (!up && (s.pair === "needed" || s.pair === "denied" || s.pair === "rejected")) {
      st = "Not connected";
      msg = s.pair === "denied" ? "Zipline turned this browser down." : s.pair === "rejected" ? "Zipline no longer knows this browser." : "Connect this browser to Zipline. Its window asks you to allow it.";
      btn = asked === "pair" ? "Asking…" : "Connect";
    } else if (!up && s.host) {
      msg = "Zipline isn't running.";
      btn = asked === "start" ? "Starting…" : "Start Zipline";
    }
    el("dot").className = "dot " + dot;
    el("st").textContent = st;
    el("st").title = up ? (s.via === "native" ? "Through the browser's native messaging link" : s.via === "http" ? "Over 127.0.0.1 (paired)" : "") : "";
    el("link").hidden = !msg;
    el("linkbox").className = "banner" + (warn ? " warn" : "");
    el("linkmsg").textContent = msg;
    el("linkbtn").hidden = !btn;
    el("linkbtn").textContent = btn;
    el("linkbtn").disabled = !!asked;
  }

  // showLink: after popup.js's ping (p = {ok}), and again whenever link.js's state changes
  globalThis.showLink = async p => {
    last = p;
    const { zlLink: s = {} } = await LB.storage.local.get({ zlLink: {} });
    draw(p, s);
  };
  el("linkbtn").onclick = () => {
    const pair = el("linkbtn").textContent === "Connect";
    asked = pair ? "pair" : "start";
    LB.storage.local.set(pair ? { zlPairNow: Date.now() } : { zlStartNow: Date.now() });
    LB.storage.local.get({ zlLink: {} }).then(r => draw(last, r.zlLink));
    setTimeout(() => { if (asked) { asked = ""; showLink(last); } }, 12000); // (nothing came of it: the button again)
  };
  LB.storage.onChanged.addListener((ch, area) => {
    if (area !== "local" || !ch.zlLink) return;
    const s = ch.zlLink.newValue || {};
    if (asked === "pair" && s.pair !== "needed" || asked === "start" && s.up) asked = "";
    last = s.up && (s.via || !s.pair) ? { ok: true } : last && last.ok && !s.up ? { ok: false } : last;
    draw(last, s);
  });
})();
