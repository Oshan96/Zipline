"""Make the browser extension's store images in assets/store/ (docs/store-listing.md).

usage: python3 tools/store_images.py

The extension's popup is rendered from extension/popup.html (Playwright + Chromium) with a stand-in for the
browser that lists two videos; the window's pictures come from assets/screenshots/ (tools/screenshots.py).
Pillow puts them together at the sizes the stores ask for:
  screenshot-*.png    1280x800   (Chrome Web Store, Edge Add-ons, addons.mozilla.org)
  promo-small.png     440x280    (the Chrome Web Store's and Edge's small promo tile)
  promo-marquee.png   1400x560   (the Chrome Web Store's marquee, optional)
The words are set in Inter when it's installed (apt install fonts-inter), else in DejaVu Sans.
"""
import functools, http.server, io, os, sys, threading

from PIL import Image, ImageDraw, ImageFilter, ImageFont
from playwright.sync_api import sync_playwright

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "assets", "store")
ACCENT, INK, MUTED, PAPER = (26, 108, 240), (28, 35, 48), (88, 98, 116), (244, 247, 252)

# the browser the popup sees: connected, two videos on a page, the usual settings
STUB = """
(() => {
  const items = [
    { url: "https://cdn.example/lecture.m3u8", kind: "hls", title: "Lecture 4: Fourier series", duration: 3172, quals: [1080, 720, 480], size: -1 },
    { url: "split:https://cdn.example/clip.mp4", kind: "split", title: "Sunrise over the harbour", duration: 94, quals: [], size: -1 },
  ];
  const reply = m => ({ ping: { ok: true }, "is-blocked": { blocked: false }, "media-list": { items },
    prefs: { prefs: { takeover: true, minKB: 256, videoButton: true, noButton: [] }, theme: "light" } })[m.type] || { ok: true };
  const on = { addListener() {} };
  window.chrome = {
    runtime: { sendMessage: async m => reply(m), onMessage: on, getManifest: () => ({ version: "1.1.0" }) },
    tabs: { query: async () => [{ id: 1, url: "https://videos.example/watch/4", title: "Lecture 4" }], create() {} },
    storage: { local: { get: async d => ({ ...d, zlLink: { up: true, via: "native" } }), set: async () => {} }, onChanged: on },
    commands: { getAll: async () => [{ name: "zl-download", shortcut: "Alt+Shift+Z" }, { name: "_execute_action", shortcut: "Alt+Shift+X" }] },
    permissions: { contains: async () => true },
  };
})();
"""


def font(size, bold=False):
    for name in (["Inter-Bold.otf", "Inter-SemiBold.otf"] if bold else ["Inter-Regular.otf"]) + (["DejaVuSans-Bold.ttf"] if bold else ["DejaVuSans.ttf"]):
        for d in ("/usr/share/fonts/opentype/inter", "/usr/share/fonts/truetype/dejavu"):
            p = os.path.join(d, name)
            if os.path.exists(p):
                return ImageFont.truetype(p, size)
    return ImageFont.load_default()


def popup_shot():
    """The popup, rendered at twice its size."""
    class Quiet(http.server.SimpleHTTPRequestHandler):
        def log_message(self, *a):
            pass
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Quiet, directory=os.path.join(ROOT, "extension")))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        with sync_playwright() as p:
            b = p.chromium.launch()
            pg = b.new_page(viewport={"width": 340, "height": 600}, device_scale_factor=2, color_scheme="light")
            pg.add_init_script(STUB)
            pg.goto(f"http://127.0.0.1:{srv.server_address[1]}/popup.html")
            pg.wait_for_selector("#videos .row")
            pg.wait_for_timeout(300)
            png = pg.locator("body").screenshot()
            b.close()
    finally:
        srv.shutdown()
    return Image.open(io.BytesIO(png)).convert("RGBA")


def shadowed(img, radius=14, blur=22, offset=10):
    """img with rounded corners on a soft shadow (the padding is part of the result)."""
    w, h = img.size
    mask = Image.new("L", (w, h), 0)
    ImageDraw.Draw(mask).rounded_rectangle((0, 0, w - 1, h - 1), radius, fill=255)
    pad = blur * 2 + offset
    out = Image.new("RGBA", (w + pad * 2, h + pad * 2), (0, 0, 0, 0))
    shadow = Image.new("RGBA", out.size, (0, 0, 0, 0))
    ImageDraw.Draw(shadow).rounded_rectangle((pad, pad + offset, pad + w, pad + h + offset), radius, fill=(20, 30, 50, 70))
    out = Image.alpha_composite(out, shadow.filter(ImageFilter.GaussianBlur(blur)))
    framed = img.copy()
    framed.putalpha(mask)
    out.alpha_composite(framed, (pad, pad))
    return out, pad


def canvas(w, h):
    img = Image.new("RGBA", (w, h), PAPER + (255,))
    d = ImageDraw.Draw(img)
    for y in range(h):  # a faint wash of the accent from the top, mixed into the paper
        a = 0.11 * (1 - y / h)
        d.line([(0, y), (w, y)], fill=tuple(round(p + (c - p) * a) for p, c in zip(PAPER, ACCENT)) + (255,))
    return img


def write(d, xy, text, size, bold=False, fill=INK, width=None):
    f = font(size, bold)
    if width is None:
        d.text(xy, text, font=f, fill=fill)
        return xy[1] + int(size * 1.25)
    words, line, y = text.split(), "", xy[1]
    for w in words:
        t = (line + " " + w).strip()
        if d.textlength(t, font=f) > width and line:
            d.text((xy[0], y), line, font=f, fill=fill)
            y += int(size * 1.35)
            line = w
        else:
            line = t
    d.text((xy[0], y), line, font=f, fill=fill)
    return y + int(size * 1.35)


def fit(img, w, h):
    s = min(w / img.width, h / img.height)
    return img.resize((round(img.width * s), round(img.height * s)), Image.LANCZOS)


def save(img, name):
    img.convert("RGB").save(os.path.join(OUT, name), optimize=True)
    print(name, img.size)


def main():
    os.makedirs(OUT, exist_ok=True)
    icon = Image.open(os.path.join(ROOT, "extension", "icons", "icon128.png")).convert("RGBA")
    pop = popup_shot()
    shots = os.path.join(ROOT, "assets", "screenshots")

    # 1: the popup and what it does
    img = canvas(1280, 800)
    d = ImageDraw.Draw(img)
    y = write(d, (90, 230), "Every video on the page, one click away", 46, True, width=560)
    y = write(d, (90, y + 18), "Zipline Download Catcher finds the videos and files a page plays or links to, and hands them to the Zipline app: many connections, pause and resume, one MP4 at the end.", 23, fill=MUTED, width=540)
    p, pad = shadowed(fit(pop, 470, 700))
    img.alpha_composite(p, (1280 - 90 - p.width + pad, (800 - p.height) // 2))
    save(img, "screenshot-popup.png")

    # 2 and 3: the app it hands them to
    for name, src, title, sub in [
        ("screenshot-window.png", "window-light.png", "Fast, resumable downloads", "Each file on many connections, checked as it arrives, picked up again after a dropped line or a restart."),
        ("screenshot-dark.png", "window-dark.png", "Queues, schedules and torrents too", "Light or dark, Windows, macOS and Linux. Free and open source, with nothing sent anywhere."),
    ]:
        img = canvas(1280, 800)
        d = ImageDraw.Draw(img)
        write(d, (90, 64), title, 44, True)
        write(d, (90, 128), sub, 22, fill=MUTED, width=1100)
        w, pad = shadowed(fit(Image.open(os.path.join(shots, src)).convert("RGBA"), 1100, 560), radius=12)
        img.alpha_composite(w, ((1280 - w.width) // 2, 800 - w.height + pad - 6))
        save(img, name)

    # the small promo tile
    img = canvas(440, 280)
    d = ImageDraw.Draw(img)
    img.alpha_composite(fit(icon, 84, 84), (40, 54))
    write(d, (140, 58), "Zipline", 40, True)
    write(d, (140, 108), "Download Catcher", 22, fill=MUTED)
    write(d, (40, 176), "Videos and downloads from your browser, faster.", 19, fill=INK, width=360)
    save(img, "promo-small.png")

    # the marquee
    img = canvas(1400, 560)
    d = ImageDraw.Draw(img)
    img.alpha_composite(fit(icon, 96, 96), (90, 150))
    write(d, (210, 152), "Zipline", 56, True)
    write(d, (210, 222), "Download Catcher", 28, fill=MUTED)
    write(d, (90, 300), "Send links, videos and downloads from your browser to the Zipline download manager.", 26, width=560)
    p, pad = shadowed(fit(pop, 400, 500))
    img.alpha_composite(p, (1400 - 120 - p.width + pad, (560 - p.height) // 2 + 4))
    save(img, "promo-marquee.png")


if __name__ == "__main__":
    sys.exit(main())
