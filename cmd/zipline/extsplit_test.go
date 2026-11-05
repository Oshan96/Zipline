package main

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// The extension's side of split videos, run in node with a stand-in for the browser: a player fetches a
// video's picture (two qualities) and sound in pieces picked in the address, and the popup offers one video,
// which goes to the app as its files, newest first, without the pieces' ranges. A minute later another video
// is another entry. A plain video fetched in Range-header pieces stays an ordinary entry until a separate sound
// file shows the picture is only half. Nothing is noted on a protected site.
func TestExtensionSplitVideos(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	script := `
const fs = require("fs"), vm = require("vm");
const store = {}, listeners = {}, sent = [];
let clock = 1.7e12;
Date.now = () => clock;
const overrides = {
  "B.storage.session.get": async k => {
    if (typeof k === "string") return { [k]: store[k] };
    const out = {};
    for (const [kk, dv] of Object.entries(k || {})) out[kk] = kk in store ? store[kk] : dv;
    return out;
  },
  "B.storage.session.set": async o => { Object.assign(store, JSON.parse(JSON.stringify(o))); },
  "B.storage.session.remove": async k => { delete store[k]; },
  "B.storage.local.get": async k => (k && typeof k === "object" ? { ...k } : {}),
  "B.action.setBadgeText": async o => { if (o.tabId) store["badge:" + o.tabId] = o.text; },
  "B.tabs.get": async id => ({ id, url: "https://videos.example/watch/2", title: "Second video - Videos" }),
  "B.scripting.executeScript": async () => [{ frameId: 0, result: { og: "Second video", tw: "", site: "", doc: "", dur: 0 } }],
  "B.cookies.getAll": async () => [{ name: "s", value: "1" }],
};
function api(path) {
  return new Proxy(function () {}, {
    get(_, k) {
      if (k === "then") return undefined;
      const p = path + "." + String(k);
      if (k === "addListener") return f => { (listeners[path] = listeners[path] || []).push(f); };
      return overrides[p] || api(p);
    },
    apply() { return Promise.resolve(undefined); },
  });
}
globalThis.chrome = api("B");
globalThis.zlLink = { call: async (path, body) => { sent.push({ path, body }); return { ok: true }; }, ping: async () => ({ config: null }) };
vm.runInThisContext(fs.readFileSync(process.argv[process.argv.length - 1], "utf8"), { filename: "background.js" });

const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(r => setImmediate(r)); };
let rid = 0;
async function fetched(tab, url, type, o = {}) {
  const id = String(++rid);
  const each = (ev, d) => (listeners["B.webRequest." + ev] || []).forEach(f => f(d));
  each("onBeforeSendHeaders", { tabId: tab, url, requestId: id,
    requestHeaders: [{ name: "Referer", value: "https://videos.example/watch/2" }, { name: "Cookie", value: "s=1" }] });
  each("onHeadersReceived", { tabId: tab, url, requestId: id, method: "GET", statusCode: o.status || 200,
    initiator: o.from || "https://videos.example", responseHeaders: [{ name: "content-type", value: type }, { name: "content-length", value: "65536" },
      ...(o.range ? [{ name: "content-range", value: o.range }] : [])] });
  await settle();
}
const ask = (type, tabId, more = {}) => new Promise(res => listeners["B.runtime.onMessage"][0]({ type, tabId, ...more }, {}, res));
const cdn = "https://cdn.example/v/";
(async () => {
  const out = {};
  // pieces picked in the address: the sound and two qualities of the picture
  for (const [f, type] of [["sd.mp4", "video/mp4"], ["snd.mp4", "audio/mp4"], ["hd.mp4", "video/mp4"]]) {
    await fetched(7, cdn + f + "?oh=a%2Fb&bytestart=0&byteend=65535", type);
    clock += 1000;
    await fetched(7, cdn + f + "?oh=a%2Fb&bytestart=65536&byteend=131071", type); // (the next piece: the same file)
    clock += 1000;
  }
  out.one = (await ask("media-list", 7)).items;
  out.badge = store["badge:7"];
  out.send = await ask("send-media", 7, { url: out.one[0].url });
  out.sent = sent.pop();
  clock += 70000; // the player was quiet: another video
  await fetched(7, cdn + "next.webm?range=0-1000", "video/webm");
  out.two = (await ask("media-list", 7)).items.map(i => i.kind + " " + i.url);

  // Range-header pieces: a plain video, until a separate sound shows up
  await fetched(8, cdn + "plain.mp4", "video/mp4", { status: 206, range: "bytes 0-65535/5000000" });
  out.plain = (await ask("media-list", 8)).items.map(i => i.kind + " " + i.url);
  await fetched(8, cdn + "plain-sound.m4a", "audio/mp4", { status: 206, range: "bytes 0-65535/900000" });
  out.paired = (await ask("media-list", 8)).items.map(i => i.kind + " " + i.url);

  await fetched(9, "https://rr1---sn-x.googlevideo.com/videoplayback?itag=137&range=0-99", "video/mp4", { from: "https://www.youtube.com" });
  await fetched(9, cdn + "x.mp4?bytestart=0&byteend=9", "video/mp4", { from: "https://www.youtube.com" });
  out.blocked = (await ask("media-list", 9)).items.length;
  out.whole = [wholeFile("https://cdn.example/v/1.mp4?oh=a%2Fb&bytestart=0&byteend=1023&oe=6"), wholeFile("https://cdn.example/a.webm?Range=1-2#t=3")];
  console.log(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.log(String(e.stack || e)); process.exit(1); });
`
	out, err := exec.Command(node, "-e", script, repoPath("extension/background.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		One []struct {
			URL, Kind, Title string
		}
		Badge string
		Send  map[string]any
		Sent  struct {
			Path string
			Body struct {
				URL, Cookie, Referer, Title string
				Split                       []string
			}
		}
		Two, Plain, Paired []string
		Blocked            int
		Whole              []string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s (%v)", out, err)
	}
	if len(got.One) != 1 || got.One[0].Kind != "split" || got.One[0].Title != "Second video" || got.Badge != "1" {
		t.Fatalf("one video: %+v (badge %q)", got.One, got.Badge)
	}
	b := got.Sent.Body
	want := []string{"https://cdn.example/v/hd.mp4?oh=a%2Fb", "https://cdn.example/v/snd.mp4?oh=a%2Fb", "https://cdn.example/v/sd.mp4?oh=a%2Fb"}
	if got.Send["ok"] != true || got.Sent.Path != "add" || b.URL != want[0] || len(b.Split) != 3 || b.Split[0] != want[0] || b.Split[1] != want[1] || b.Split[2] != want[2] ||
		b.Referer != "https://videos.example/watch/2" || b.Cookie == "" || b.Title != "Second video" {
		t.Fatalf("sent %+v %+v", got.Send, got.Sent)
	}
	if len(got.Two) != 2 || got.Two[0] != "split split:https://cdn.example/v/next.webm" {
		t.Errorf("another video: %v", got.Two)
	}
	if len(got.Plain) != 1 || got.Plain[0] != "video https://cdn.example/v/plain.mp4" {
		t.Errorf("a plain video: %v", got.Plain)
	}
	if len(got.Paired) != 1 || got.Paired[0] != "split split:https://cdn.example/v/plain.mp4" {
		t.Errorf("picture and sound: %v", got.Paired)
	}
	if got.Blocked != 0 {
		t.Errorf("%d entries on a protected site", got.Blocked)
	}
	if got.Whole[0] != "https://cdn.example/v/1.mp4?oh=a%2Fb&oe=6" || got.Whole[1] != "https://cdn.example/a.webm" || wholeFile("https://cdn.example/v/1.mp4?oh=a%2Fb&bytestart=0&byteend=1023&oe=6") != got.Whole[0] {
		t.Errorf("whole files: %v", got.Whole)
	}
}
