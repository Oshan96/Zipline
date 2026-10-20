#!/usr/bin/env python3
"""Takes the screenshots in assets/screenshots/ for the README and the user guide.

    python3 tools/screenshots.py [path/to/zipline-binary]

Builds the app (unless a binary is given) and starts it on a private config folder. A small local web
proxy stands in for the internet: Zipline's proxy setting points at it, and it makes up each file's
bytes as they're asked for and sends them at a set speed per connection. So the downloads in the
pictures are real ones, with real progress, speeds and connections, from the addresses they'd have,
and nothing leaves the computer. Then Playwright for Python and Chromium photograph the window, in the
light and the dark theme, and a download's progress window.
"""
import hashlib, http.server, json, os, random, shutil, socketserver, subprocess, sys, tempfile, threading, time
import urllib.parse, urllib.request
from playwright.sync_api import sync_playwright

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "assets", "screenshots")
PORT = int(os.environ.get("ZL_PORT", "17993"))
BASE = f"http://127.0.0.1:{PORT}"
NOPROXY = urllib.request.build_opener(urllib.request.ProxyHandler({}))
MB = 1 << 20

# address: (size, bytes per second per connection; 0 = as fast as it goes)
FILES = {
    "http://releases.ubuntu.com/24.04.1/ubuntu-24.04.1-desktop-amd64.iso": (6_114_656_256, 700_000),
    "http://download.blender.org/release/Blender4.2/blender-4.2.3-windows-x64.msi": (361 * MB, 420_000),
    "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_2160p_60fps_normal.mp4": (642 * MB, 520_000),
    "http://download.fedoraproject.org/pub/fedora/linux/releases/41/Workstation/x86_64/iso/Fedora-Workstation-Live-x86_64-41-1.4.iso": (2_288 * MB, 4_000_000),
    "http://download.documentfoundation.org/libreoffice/stable/24.8.2/win/x86_64/LibreOffice_24.8.2_Win_x86-64.msi": (349 * MB, 0),
    "http://archive.org/download/pride_and_prejudice_librivox/prideandprejudice_01-02_austen.mp3": (31 * MB, 0),
    "http://www.gnu.org/software/emacs/manual/pdf/emacs.pdf": (9 * MB, 0),
    "http://nodejs.org/dist/v22.11.0/node-v22.11.0-linux-x64.tar.xz": (29 * MB, 0),
    "http://download.kde.org/stable/kdenlive/24.08/linux/kdenlive-24.08.2-x86_64.AppImage": (115 * MB, 0),
}
NAMES = {u.rsplit("/", 1)[-1]: u for u in FILES}


def block(name):
    """The 64 KiB that the made-up file repeats (different for each file)."""
    return random.Random(name).randbytes(1 << 16)


def sha256(url):
    size, b = FILES[url][0], block(url)
    h = hashlib.sha256()
    for _ in range(size // len(b)):
        h.update(b)
    h.update(b[: size % len(b)])
    return h.hexdigest()


class Files(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_HEAD(self):
        self.serve(False)

    def do_GET(self):
        self.serve(True)

    def serve(self, body):
        u = urllib.parse.urlsplit(self.path)  # a proxy is asked for the whole address
        url = f"http://{u.netloc}{u.path}"
        if url not in FILES:
            self.send_error(404)
            return
        size, rate = FILES[url]
        start, end = 0, size - 1
        rng = self.headers.get("Range", "")
        if rng.startswith("bytes="):
            a, _, b = rng[6:].partition("-")
            start = int(a) if a else size - int(b)
            end = min(int(b), size - 1) if a and b else end
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        else:
            self.send_response(200)
        self.send_header("Content-Length", str(end - start + 1))
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("ETag", '"' + hashlib.md5(url.encode()).hexdigest() + '"')
        self.end_headers()
        if not body:
            return
        b, pos, t0, sent = block(url), start, time.monotonic(), 0
        try:
            while pos <= end:
                o = pos % len(b)
                n = min(len(b) - o, end - pos + 1, 16384 if rate else len(b))
                self.wfile.write(b[o:o + n])
                pos, sent = pos + n, sent + n
                if rate:
                    ahead = sent / rate - (time.monotonic() - t0)
                    if ahead > 0:
                        time.sleep(ahead)
        except (BrokenPipeError, ConnectionResetError):
            pass


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def handle_error(self, request, client_address):
        pass  # (Zipline closes connections it no longer needs)


class App:
    def __init__(self, binary, proxy):
        self.home = tempfile.mkdtemp(prefix="zl-shots-")
        self.env = {k: v for k, v in os.environ.items() if "proxy" not in k.lower()}
        self.env.update(HOME=self.home, XDG_CONFIG_HOME=os.path.join(self.home, "cfg"), ZL_PORT=str(PORT))
        data = os.path.join(self.home, "cfg", "Zipline")
        os.makedirs(data)
        with open(os.path.join(data, "settings.json"), "w") as f:
            json.dump({"downloadDir": os.path.join(self.home, "Downloads"), "clipboardMonitor": False, "showCompleteDialog": False,
                       "tray": False, "keepAwake": False, "earlyStart": False, "maxConcurrent": 6,
                       "proxy": {"mode": "manual", "url": proxy}}, f)
        self.bin = binary
        self.p = subprocess.Popen([binary, "--nowindow"], env=self.env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            try:
                NOPROXY.open(BASE + "/", timeout=1)
                break
            except Exception:
                time.sleep(0.1)
        with open(os.path.join(data, "run.json")) as f:
            self.secret = json.load(f)["secret"]

    def api(self, path, body):
        req = urllib.request.Request(BASE + "/api/" + path, data=json.dumps(body).encode(),
                                     headers={"X-Token": self.secret, "Content-Type": "application/json"})
        return json.loads(NOPROXY.open(req).read())

    def login_url(self):
        return subprocess.run([self.bin, "--login-url"], env=self.env, capture_output=True, text=True, check=True).stdout.strip()

    def stop(self):
        subprocess.run([self.bin, "--quit"], env=self.env, capture_output=True)
        try:
            self.p.wait(10)
        except Exception:
            self.p.kill()
        shutil.rmtree(self.home, ignore_errors=True)


def status(pg, rid):
    return pg.evaluate(f"(S.byId.get({json.dumps(rid)}) || {{}}).status")


def wait_for(cond, secs=60):
    t = time.time()
    while time.time() - t < secs:
        if cond():
            return
        time.sleep(0.25)
    raise RuntimeError("timed out")


def shoot(pg, name, **kw):
    path = os.path.join(OUT, name)
    pg.screenshot(path=path, **kw)
    print("wrote", os.path.relpath(path, ROOT))


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else None
    tmp = None
    if not binary:
        tmp = tempfile.mkdtemp(prefix="zl-bin-")
        binary = os.path.join(tmp, "zipline")
        subprocess.run(["go", "build", "-o", binary, "./cmd/zipline"], cwd=ROOT, check=True)
    srv = Server(("127.0.0.1", 0), Files)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    os.makedirs(OUT, exist_ok=True)
    app = App(binary, f"http://127.0.0.1:{srv.server_address[1]}")
    try:
        ids = {}
        # the finished ones first, so they sit lower in the list (newest first)
        done = ["emacs.pdf", "node-v22.11.0-linux-x64.tar.xz", "prideandprejudice_01-02_austen.mp3", "LibreOffice_24.8.2_Win_x86-64.msi"]
        for n in done:
            req = {"url": NAMES[n], "start": True}
            if n.endswith(".msi"):
                req["checksum"] = sha256(NAMES[n])  # shows as "Complete · verified"
            ids[n] = app.api("add", req)["id"]
        fedora = "Fedora-Workstation-Live-x86_64-41-1.4.iso"
        ids["kdenlive"] = app.api("add", {"url": NAMES["kdenlive-24.08.2-x86_64.AppImage"], "start": False, "queueId": "main"})["id"]
        ids[fedora] = app.api("add", {"url": NAMES[fedora], "start": True})["id"]
        for n in ["bbb_sunflower_2160p_60fps_normal.mp4", "blender-4.2.3-windows-x64.msi", "ubuntu-24.04.1-desktop-amd64.iso"]:
            ids[n] = app.api("add", {"url": NAMES[n], "start": True})["id"]
            time.sleep(0.4)
        ubuntu = ids["ubuntu-24.04.1-desktop-amd64.iso"]
        with sync_playwright() as p:
            b = p.chromium.launch()
            for scheme in ["light", "dark"]:
                ctx = b.new_context(viewport={"width": 1280, "height": 600}, device_scale_factor=2, color_scheme=scheme)
                pg = ctx.new_page()
                pg.goto(app.login_url())
                pg.wait_for_selector("#tbody tr[data-id]")
                if scheme == "light":
                    wait_for(lambda: all(status(pg, ids[n]) == "completed" for n in done), 120)
                    got = lambda: pg.evaluate(f"(S.byId.get({json.dumps(ids[fedora])}) || {{}}).done || 0")
                    wait_for(lambda: got() > 0.4 * FILES[NAMES[fedora]][0], 180)
                    app.api("action", {"action": "pause", "ids": [ids[fedora]]})
                    time.sleep(15)  # the speeds settle and every download has all its connections
                pg.evaluate("document.querySelector('#tablewrap').scrollTop = 0")
                pg.click(f"#tbody tr[data-id='{ubuntu}']")
                time.sleep(1.5)
                shoot(pg, f"window-{scheme}.png")
                if scheme == "light":
                    pg.evaluate(f"progressDialog({json.dumps(ubuntu)})")
                    time.sleep(4)
                    dlg = pg.locator("dialog[open], .dlg:not(dialog)").last
                    shoot(pg, "progress.png", clip=dlg.bounding_box())
                    pg.keyboard.press("Escape")
                ctx.close()
            b.close()
    finally:
        app.stop()
        srv.shutdown()
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
