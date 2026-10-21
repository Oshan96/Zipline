#!/usr/bin/env python3
"""Browser tests for the download list: live updates and full keyboard use.

    python3 tools/ui_list_test.py [path/to/zipline-binary]

Builds the app (unless a binary is given), starts it on a private config folder with a list of
3,000 downloads (ZL_PORT, default 17991), and drives the window with Playwright for Python and
Chromium: Tab order, the menu bar, the context menu from the keyboard, the list as a grid with
virtual scrolling, column headers, the sidebar, the global shortcuts, the mouse still working,
and the live updates (deltas, resync, segments for open progress windows, slower when hidden).
Exits non-zero when a test fails.
"""
import datetime, json, os, shutil, subprocess, sys, tempfile, time, traceback, urllib.request
from playwright.sync_api import sync_playwright

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PORT = int(os.environ.get("ZL_PORT", "17991"))
BASE = f"http://127.0.0.1:{PORT}"
N = 3000
SECRET = ""


def make_list(datadir, n):
    os.makedirs(datadir, exist_ok=True)
    cats = {"Compressed": "zip", "Documents": "pdf", "Music": "mp3", "Programs": "exe", "Video": "mp4", "Images": "png", "General": "bin"}
    now = datetime.datetime.now(datetime.timezone.utc)
    out = []
    for i in range(n):
        cat = list(cats)[i % 7]
        name = f"file-{i:05d}.{cats[cat]}"
        size = 1_000_000 + (i * 7919) % 900_000_000
        status = ["completed", "completed", "paused", "queued", "error"][i % 5]
        added = (now - datetime.timedelta(minutes=n - i)).isoformat().replace("+00:00", "Z")
        d = {"id": f"t{i:06x}", "url": f"https://example.com/{name}", "name": name, "dir": "/tmp/zl", "category": cat, "size": size,
             "resumable": True, "status": status, "added": added, "conns": 4}
        if status != "completed":
            d["segments"] = [{"s": 0, "e": size - 1, "p": size // 3}]
        if status == "queued":
            d["queue"] = "main"
        if status == "error":
            d["error"] = "the server closed the connection"
        out.append(d)
    with open(os.path.join(datadir, "downloads.json"), "w") as f:
        json.dump({"format": 2, "downloads": out}, f)
    with open(os.path.join(datadir, "settings.json"), "w") as f:
        json.dump({"downloadDir": "/tmp/zl", "clipboardMonitor": False, "showCompleteDialog": False, "tray": False, "keepAwake": False, "earlyStart": False}, f)


class App:
    def __init__(self, binary):
        self.home = tempfile.mkdtemp(prefix="zl-ui-")
        self.env = dict(os.environ, HOME=self.home, XDG_CONFIG_HOME=os.path.join(self.home, "cfg"), ZL_PORT=str(PORT))
        make_list(os.path.join(self.home, "cfg", "Zipline"), N)
        self.bin = binary
        self.p = subprocess.Popen([binary, "--nowindow"], env=self.env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            try:
                urllib.request.urlopen(BASE + "/", timeout=1)
                return
            except Exception:
                time.sleep(0.1)
        raise RuntimeError("the app didn't start")

    def secret(self):
        """run.json's secret: how programs (and this script) call the app's API."""
        with open(os.path.join(self.home, "cfg", "Zipline", "run.json")) as f:
            return json.load(f)["secret"]

    def login_url(self):
        """A one-time address that signs a window in (the page itself has no token)."""
        return subprocess.run([self.bin, "--login-url"], env=self.env, capture_output=True, text=True, check=True).stdout.strip()

    def stop(self):
        subprocess.run([self.bin, "--quit"], env=self.env, capture_output=True)
        try:
            self.p.wait(10)
        except Exception:
            self.p.kill()
        shutil.rmtree(self.home, ignore_errors=True)


# ---------------------------------------------------------------- helpers

def active(pg):
    """What has the focus: an id, or role:text."""
    return pg.evaluate("""() => { const a = document.activeElement; if (!a || a === document.body) return 'body';
        if (a.id) return '#' + a.id; return (a.getAttribute('role') || { TH: 'columnheader', BUTTON: 'button', INPUT: 'input' }[a.tagName] || a.tagName.toLowerCase()) + ':' + a.textContent.trim().slice(0, 40); }""")


def focus_grid(pg):
    pg.evaluate("document.querySelector('#tbl').focus()")


def ev(pg, js):
    return pg.evaluate(js)


def check(cond, msg):
    if not cond:
        raise AssertionError(msg)


DIALOGS = "[...document.querySelectorAll('.dlg:not(dialog), dialog[open]')]"


def dialog_count(pg):
    return ev(pg, DIALOGS + ".length")


def dialog_titles(pg):
    return ev(pg, DIALOGS + ".map(d => (d.querySelector('.t, h1, h2') || d).textContent.trim())")


def close_dialogs(pg):
    for _ in range(5):
        if not dialog_count(pg):
            return
        pg.keyboard.press("Escape")
        time.sleep(0.1)


def fresh(pg):
    """Back to a known state: all downloads, sorted by date added, top of the list, grid focused."""
    close_dialogs(pg)
    ev(pg, "closeMenus(true); S.filter = 'all'; S.search = ''; document.querySelector('#search').value = ''; S.sort = { col: 'added', dir: 1 }; S.selected.clear(); S.anchor = null; LIST.focus = null; buildHead(); renderAll(); document.querySelector('#tablewrap').scrollTop = 0")
    time.sleep(0.15)
    focus_grid(pg)


def cur_index(pg):
    return ev(pg, "idx(LIST.focus)")


def row_visible(pg, rid):
    return ev(pg, f"""(() => {{ const r = document.getElementById({json.dumps(rid)}); if (!r) return false;
        const b = r.getBoundingClientRect(), w = document.querySelector('#tablewrap').getBoundingClientRect(), h = document.querySelector('#thead').getBoundingClientRect();
        return b.top >= h.bottom - 1 && b.bottom <= w.bottom + 1; }})()""")


# ---------------------------------------------------------------- tests

def test_tab_order(pg):
    pg.reload()
    pg.wait_for_selector("#tbody tr[data-id]")
    seen = []
    for _ in range(40):
        pg.keyboard.press("Tab")
        a = active(pg)
        seen.append(a)
        if a.startswith("columnheader"):
            break
    first = seen[0]
    check(first.startswith("menuitem:File"), f"Tab starts at the menu bar, got {seen[:3]}")
    check(sum(1 for s in seen if s.startswith("menuitem:")) == 1, f"the menu bar is one Tab stop: {seen}")
    i_side, i_new, i_grid = seen.index("#sbList"), next(i for i, s in enumerate(seen) if "New queue" in s), seen.index("#tbl")
    check(i_side < i_new < i_grid < len(seen) - 1, f"sidebar, New queue, list, then column headers: {seen}")
    check(seen[-1].endswith("File name"), f"the headers' Tab stop: {seen[-1]}")
    pg.keyboard.press("Shift+Tab")
    check(active(pg) == "#tbl", "Shift+Tab goes back to the list")


def test_menubar(pg):
    fresh(pg)
    k = pg.keyboard
    k.press("F10")
    check(active(pg) == "menuitem:File", f"F10 focuses the menu bar: {active(pg)}")
    k.press("ArrowRight")
    check(active(pg) == "menuitem:Downloads", "Right moves to the next menu")
    k.press("ArrowLeft"); k.press("ArrowLeft")
    check(active(pg) == "menuitem:Help", "Left wraps around to the last menu")
    k.press("ArrowLeft")
    check(active(pg) == "menuitem:Tasks", "Left again: Tasks, before Help")
    k.press("ArrowDown")
    check(ev(pg, "document.querySelector('.mi.open .mi-l').textContent") == "Tasks", "Down opens the menu")
    check(ev(pg, "document.querySelector('.mi.open .mi-l').getAttribute('aria-expanded')") == "true", "aria-expanded is set")
    check(active(pg).startswith("menuitem:Add link"), f"…and focuses its first item: {active(pg)}")
    k.press("End")
    check(active(pg).startswith("menuitem:Exit Zipline"), "End: the last item")
    k.press("ArrowDown")
    check(active(pg).startswith("menuitem:Add link"), "Down wraps to the first item")
    k.press("ArrowUp"); k.press("ArrowUp")
    check(active(pg).startswith("menuitem:When all downloads finish"), f"Up: {active(pg)}")
    k.press("ArrowRight")
    check(ev(pg, "getComputedStyle(document.querySelector('#ctx')).display") == "block" and ev(pg, "!!document.querySelector('#ctx').dataset.sub"), "Right opens the submenu")
    check(ev(pg, "document.querySelector('#ctx').contains(document.activeElement)"), "…and focuses it")
    check(ev(pg, "document.querySelector('.dd-item.sub-open').getAttribute('aria-expanded')") == "true", "the submenu's item says it's expanded")
    k.press("ArrowDown")
    k.press("ArrowLeft")
    check(ev(pg, "getComputedStyle(document.querySelector('#ctx')).display") == "none", "Left closes the submenu")
    check(active(pg).startswith("menuitem:When all downloads finish"), "…back on its item")
    k.press("ArrowRight"); k.press("ArrowRight")
    check(ev(pg, "document.querySelector('.mi.open .mi-l').textContent") == "Help", "Right in a submenu goes on to the next menu (Help)")
    k.press("Escape")
    check(not ev(pg, "!!document.querySelector('.mi.open')") and active(pg) == "menuitem:Help", "Escape closes the menu, focus on its title")
    k.press("Escape")
    check(active(pg) == "#tbl", f"Escape again: focus back where it was: {active(pg)}")
    # Alt on its own, and back
    k.down("Alt"); k.up("Alt")
    check(active(pg).startswith("menuitem:"), "Alt focuses the menu bar")
    k.down("Alt"); k.up("Alt")
    check(active(pg) == "#tbl", "Alt again goes back")
    # type-ahead, and choosing an item
    k.press("F10"); k.press("v")
    check(active(pg) == "menuitem:View", "a letter jumps to the menu starting with it")
    k.press("Enter"); k.press("c")
    check(active(pg).startswith("menuitem:Columns"), f"a letter jumps to the item: {active(pg)}")
    k.press("Enter")
    time.sleep(0.2)
    check("Columns" in dialog_titles(pg), "Enter chooses it (the Columns window)")
    check(not ev(pg, "!!document.querySelector('.mi.open')"), "the menu closed")
    close_dialogs(pg)
    # disabled items can be reached but not chosen
    fresh(pg)
    k.press("F10"); k.press("Enter")
    check(ev(pg, "document.activeElement.getAttribute('aria-disabled')") == "true", "File → Open is disabled with nothing selected")
    k.press("Enter")
    check(ev(pg, "!!document.querySelector('.mi.open')"), "a disabled item does nothing")
    k.press("Tab")
    check(not ev(pg, "!!document.querySelector('.mi.open')") and active(pg) == "#tbl", "Tab closes the menus and goes back")
    # opened with the mouse, then the keyboard
    pg.click("#menubar .mi-l >> text=Help")
    k.press("ArrowDown")
    check(active(pg).startswith("menuitem:Browser integration"), f"keys work in a menu opened by the mouse: {active(pg)}")
    k.press("Escape"); k.press("Escape")


def test_context_menu(pg):
    fresh(pg)
    k = pg.keyboard
    k.press("Home"); k.press("ArrowDown"); k.press("ArrowDown")
    rid = ev(pg, "'r-' + LIST.focus")
    k.press("Shift+F10")
    check(ev(pg, "getComputedStyle(document.querySelector('#ctx')).display") == "block", "Shift+F10 opens the context menu")
    check(active(pg).startswith("menuitem:Open"), f"…focused on its first item: {active(pg)}")
    top = ev(pg, "document.querySelector('#ctx').getBoundingClientRect().top")
    rb = ev(pg, f"document.getElementById('{rid}').getBoundingClientRect().bottom")
    check(abs(top - rb) < 2, f"…at the focused row ({top} vs {rb})")
    k.press("ArrowDown")
    check(active(pg).startswith("menuitem:Open folder"), "arrows move in it")
    k.press("Escape")
    check(ev(pg, "getComputedStyle(document.querySelector('#ctx')).display") == "none" and active(pg) == "#tbl", "Escape closes it, focus back on the list")
    check(ev(pg, "'r-' + LIST.focus") == rid, "…on the same row")
    # the Menu key, on a row that isn't selected: it gets selected
    k.press("Control+ArrowDown")
    fid = ev(pg, "LIST.focus")
    check(not ev(pg, f"S.selected.has('{fid}')"), "Ctrl+Down leaves the selection")
    k.press("ContextMenu")
    check(ev(pg, "getComputedStyle(document.querySelector('#ctx')).display") == "block", "the Menu key opens it")
    check(ev(pg, f"S.selected.size === 1 && S.selected.has('{fid}')"), "…for the focused row")
    k.press("End")
    check(active(pg).startswith("menuitem:Properties"), "End: the last item")
    k.press("Enter")
    time.sleep(0.4)
    check(dialog_count(pg) >= 1, "Enter chooses it (Properties)")
    close_dialogs(pg)


def test_grid(pg):
    fresh(pg)
    k = pg.keyboard
    n = ev(pg, "listIds().length")
    check(n == N, f"{N} rows in view, got {n}")
    check(ev(pg, "document.querySelector('#tbl').getAttribute('role')") == "grid" and ev(pg, "document.querySelector('#tbl').getAttribute('aria-rowcount')") == str(N + 1), "a grid with aria-rowcount")
    check(ev(pg, "document.querySelectorAll('#tbody tr[data-id]').length") < 80, "only the rows on screen are in the page")
    k.press("Home")
    first = ev(pg, "LIST.ids[0]")
    check(ev(pg, "document.querySelector('#tbl').getAttribute('aria-activedescendant')") == "r-" + first, "Home: the first row is active")
    check(ev(pg, f"document.getElementById('r-{first}').getAttribute('aria-rowindex')") == "2", "aria-rowindex counts the header")
    k.press("End")
    last = ev(pg, "LIST.ids[LIST.ids.length - 1]")
    check(ev(pg, "document.querySelector('#tbl').getAttribute('aria-activedescendant')") == "r-" + last, "End: the last row")
    time.sleep(0.1)
    check(row_visible(pg, "r-" + last), "…scrolled into view")
    check(ev(pg, f"document.getElementById('r-{last}').getAttribute('aria-rowindex')") == str(N + 1), "…its aria-rowindex")
    check(ev(pg, f"document.getElementById('r-{last}').getAttribute('aria-selected')") == "true", "…selected")
    check(ev(pg, "document.querySelector('#tbody tr.cur') && document.querySelector('#tbody tr.cur').dataset.id") == last, "…marked as the focused row")
    check(ev(pg, "getComputedStyle(document.querySelector('#tbody tr.cur')).outlineStyle") == "solid", "…with a focus outline")
    page = ev(pg, "Math.floor((document.querySelector('#tablewrap').clientHeight - document.querySelector('#thead').offsetHeight) / LIST.rowH) - 1")
    k.press("PageUp")
    check(cur_index(pg) == N - 1 - page, f"Page Up moves a page ({cur_index(pg)})")
    k.press("PageDown")
    check(cur_index(pg) == N - 1, "Page Down back")
    k.press("Home")
    check(cur_index(pg) == 0 and ev(pg, "document.querySelector('#tablewrap').scrollTop") == 0, "Home: top of the list")
    for _ in range(120):
        k.press("ArrowDown")
    check(cur_index(pg) == 120, f"120 × Down: row 120 ({cur_index(pg)})")
    time.sleep(0.1)
    check(row_visible(pg, ev(pg, "'r-' + LIST.focus")), "the focused row stays in view")
    check(ev(pg, "S.selected.size") == 1, "Down selects just that row")
    for _ in range(3):
        k.press("Shift+ArrowDown")
    check(ev(pg, "S.selected.size") == 4 and ev(pg, "document.querySelectorAll('#tbody tr[aria-selected=true]').length") == 4, "Shift+Down extends the selection (aria-selected)")
    k.press("Shift+ArrowUp")
    check(ev(pg, "S.selected.size") == 3, "Shift+Up shrinks it")
    k.press("Control+ArrowDown"); k.press("Control+ArrowDown")
    check(ev(pg, "S.selected.size") == 3 and cur_index(pg) == 124, "Ctrl+Down moves without selecting")
    k.press("Control+Space")
    check(ev(pg, "S.selected.size") == 4 and ev(pg, "S.selected.has(LIST.focus)"), "Ctrl+Space adds the row")
    k.press("Control+Space")
    check(ev(pg, "S.selected.size") == 3, "…and takes it out again")
    k.press("Control+a")
    check(ev(pg, "S.selected.size") == N, "Ctrl+A selects them all")
    # Space pauses or resumes the selection; Enter opens it (a progress window here)
    k.press("Home")
    k.press("ArrowDown"); k.press("ArrowDown")  # row 2: a paused download
    check(ev(pg, "S.byId.get(LIST.focus).status") == "paused", "row 2 is a paused download")
    with pg.expect_request(lambda r: r.url.endswith("/api/action") and json.loads(r.post_data)["action"] == "start") as rq:
        k.press(" ")
    check(json.loads(rq.value.post_data)["ids"] == [ev(pg, "LIST.focus")], "Space resumes the selected download")
    k.press("Enter")
    time.sleep(0.3)
    check(ev(pg, "S.progress.has(LIST.focus)"), "Enter opens its progress window")
    close_dialogs(pg)
    focus_grid(pg)
    k.press("Delete")
    time.sleep(0.2)
    check("Remove from list" in dialog_titles(pg), "Delete asks to remove it")
    close_dialogs(pg)
    focus_grid(pg)
    k.press("Shift+Delete")
    time.sleep(0.2)
    check("Delete file" in dialog_titles(pg), "Shift+Delete asks to delete the file")
    close_dialogs(pg)
    focus_grid(pg)
    k.press("Alt+Enter")
    time.sleep(0.4)
    check(dialog_count(pg) >= 1, "Alt+Enter: properties")
    close_dialogs(pg)


def test_headers(pg):
    fresh(pg)
    k = pg.keyboard
    k.press("Home"); k.press("ArrowUp")
    check(active(pg).startswith("columnheader:File name"), f"Up from the first row: the column headers ({active(pg)})")
    k.press("ArrowRight")
    check(active(pg).startswith("columnheader:Size"), "Right: the next header")
    k.press("Enter")
    check(ev(pg, "S.sort.col") == "size" and ev(pg, "document.querySelector('th[data-k=size]').getAttribute('aria-sort')") == "ascending", "Enter sorts (aria-sort ascending)")
    check(active(pg).startswith("columnheader:Size"), "focus stays on the header")
    k.press(" ")
    check(ev(pg, "document.querySelector('th[data-k=size]').getAttribute('aria-sort')") == "descending", "Space sorts the other way")
    sizes = ev(pg, "LIST.ids.slice(0, 50).map(id => S.byId.get(id).size)")
    check(sizes == sorted(sizes, reverse=True), "the list is sorted by size, largest first")
    check(ev(pg, "document.querySelectorAll('th[aria-sort]').length") == 1, "only the sorted column has aria-sort")
    k.press("End")
    check(active(pg).startswith("columnheader:Description"), f"End: the last header ({active(pg)})")
    k.press("Home")
    check(active(pg).startswith("columnheader:File name"), "Home: the first")
    k.press("ArrowDown")
    check(active(pg) == "#tbl", "Down: back to the rows")
    k.press("Shift+Tab")
    k.press("Tab"); k.press("Tab")
    check(active(pg).startswith("columnheader:File name"), "the headers are a Tab stop after the list")


def test_sidebar(pg):
    fresh(pg)
    k = pg.keyboard
    ev(pg, "document.querySelector('#sbList').focus()")
    check(ev(pg, "document.querySelector('#sbList').getAttribute('role')") == "listbox", "the sidebar is a listbox")
    k.press("ArrowDown")
    check(ev(pg, "S.filter") == "cat:Compressed", "Down picks the next view")
    check(ev(pg, "document.querySelector('#sbList').getAttribute('aria-activedescendant')") == "sb-cat_Compressed", "…its active option")
    check(ev(pg, "document.getElementById('sb-cat_Compressed').getAttribute('aria-selected')") == "true", "…selected")
    check(ev(pg, "listIds().length") == ev(pg, "S.downloads.filter(d => d.category === 'Compressed').length"), "the list follows")
    k.press("End")
    check(ev(pg, "S.filter") == "q:main", "End: the last queue")
    k.press("Home")
    check(ev(pg, "S.filter") == "all", "Home: all downloads")
    k.press("ArrowDown"); k.press("ArrowUp")
    check(ev(pg, "S.filter") == "all", "Up")
    ev(pg, "(() => { window.__acts = 0; const f = window.fetch; window.fetch = (u, o) => { if (String(u).includes('/api/action')) window.__acts++; return f(u, o); }; })()")
    k.press(" "); k.press("Enter"); k.press("Delete")
    time.sleep(0.2)
    check(ev(pg, "window.__acts") == 0 and dialog_count(pg) == 0, "the list's keys don't act from the sidebar")
    k.press("Tab")
    check("New queue" in active(pg), "Tab: the New queue button")


def test_shortcuts(pg):
    fresh(pg)
    k = pg.keyboard

    def opens(key, title_has, sel=None):
        k.press(key)
        time.sleep(0.4)
        titles = dialog_titles(pg)
        check(any(title_has in t for t in titles), f"{key} opens “{title_has}”: {titles}")
        k.press("Escape")
        time.sleep(0.1)
        check(not dialog_count(pg), f"Escape closes the window opened by {key}")
        focus_grid(pg)

    opens("Control+n", "Add link")
    opens("Control+b", "atch")
    opens("Control+g", "rabber")
    with pg.expect_file_chooser() as fc:
        k.press("Control+o")
    check(fc.value is not None, "Ctrl+O picks a torrent file")
    focus_grid(pg)
    k.press("Control+f")
    check(active(pg) == "#search", "Ctrl+F: the search box")
    k.type("file-0001")
    time.sleep(0.1)
    check(ev(pg, "listIds().length") == 10, f"searching filters the list ({ev(pg, 'listIds().length')})")
    sel_before = ev(pg, "S.selected.size")
    k.press("ArrowDown"); k.press("Delete"); k.press("Control+n"); k.press("Control+a"); k.press(" ")
    time.sleep(0.3)
    check(dialog_count(pg) == 0 and ev(pg, "S.selected.size") == sel_before, "typing in the search box doesn't drive the list")
    ev(pg, "document.querySelector('#search').value = ''; S.search = ''; renderTable()")
    # inside a dialog the list's shortcuts stay quiet
    focus_grid(pg)
    k.press("Control+n")
    time.sleep(0.3)
    k.press("Control+b"); k.press("Delete")
    time.sleep(0.2)
    check(dialog_count(pg) == 1, "shortcuts don't fire inside a dialog")
    k.press("Escape")
    # Ctrl+V: a pasted link
    focus_grid(pg)
    ev(pg, "(() => { const dt = new DataTransfer(); dt.setData('text', 'https://example.com/pasted.zip'); document.querySelector('#tbl').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true })); })()")
    time.sleep(0.3)
    check(ev(pg, "(document.querySelector('#u') || {}).value") == "https://example.com/pasted.zip", "Ctrl+V adds the pasted link")
    close_dialogs(pg)


def test_mouse(pg):
    fresh(pg)
    rows = lambda: ev(pg, "[...document.querySelectorAll('#tbody tr[data-id]')].map(r => r.dataset.id)")
    r = rows()
    pg.click(f"#r-{r[2]} td:nth-child(2)")
    check(ev(pg, "[...S.selected]") == [r[2]] and ev(pg, "LIST.focus") == r[2], "a click selects a row")
    pg.click(f"#r-{r[5]} td:nth-child(2)", modifiers=["Shift"])
    check(ev(pg, "S.selected.size") == 4, "Shift+click selects the range")
    pg.click(f"#r-{r[8]} td:nth-child(2)", modifiers=["Control"])
    check(ev(pg, "S.selected.size") == 5, "Ctrl+click adds one")
    pg.click(f"#r-{r[8]} td:nth-child(2)", button="right")
    check(ev(pg, "getComputedStyle(document.querySelector('#ctx')).display") == "block" and ev(pg, "S.selected.size") == 5, "right-click: the context menu for the selection")
    pg.keyboard.press("ArrowDown")
    check(active(pg).startswith("menuitem:"), "its items take the keys")
    pg.keyboard.press("Escape")
    pg.click("th[data-k=size]")
    check(ev(pg, "S.sort.col") == "size", "a header click sorts")
    # drag the Size header after Status
    a = pg.locator("th[data-k=size]").bounding_box(); b = pg.locator("th[data-k=status]").bounding_box()
    pg.mouse.move(a["x"] + 20, a["y"] + 10); pg.mouse.down(); pg.mouse.move(a["x"] + 60, a["y"] + 10, steps=4)
    pg.mouse.move(b["x"] + b["width"] - 5, b["y"] + 10, steps=6); pg.mouse.up()
    order = ev(pg, "[...document.querySelectorAll('#thead th')].map(t => t.dataset.k)")
    check(order.index("size") == order.index("status") + 1, f"dragging a header moves the column: {order}")
    check(ev(pg, "[...document.querySelectorAll('#tbody tr[data-id]')].every(tr => tr.children.length === document.querySelectorAll('#thead th').length)"), "rows match the new columns")
    check(ev(pg, f"document.querySelector('#tbody tr[data-id] td:nth-child({order.index('status') + 1})').className").startswith("st"), "…in the new order")
    # resize a column
    w0 = ev(pg, "document.querySelector('th[data-k=status]').getBoundingClientRect().width")
    rs = pg.locator("th[data-k=status] .rs").bounding_box()
    pg.mouse.move(rs["x"] + 3, rs["y"] + 5); pg.mouse.down(); pg.mouse.move(rs["x"] + 63, rs["y"] + 5, steps=5); pg.mouse.up()
    check(abs(ev(pg, "document.querySelector('th[data-k=status]').getBoundingClientRect().width") - w0 - 60) < 3, "dragging a header's edge resizes it")
    ev(pg, "saveColPrefs(null); COLW = {}; saveUI(); refreshColumns()")
    # the scroll bar doesn't clear the selection; empty space does
    ev(pg, "S.selected = new Set([LIST.ids[0]]); renderTable()")
    wr = pg.locator("#tablewrap").bounding_box()
    sbw = ev(pg, "(() => { const w = document.querySelector('#tablewrap'); return w.offsetWidth - w.clientWidth; })()")
    if sbw > 0:
        pg.mouse.click(wr["x"] + wr["width"] - sbw / 2, wr["y"] + wr["height"] / 2)
        check(ev(pg, "S.selected.size") == 1, "clicking the scroll bar keeps the selection")
    # mouse wheel scrolling draws the rows coming into view
    pg.mouse.move(wr["x"] + 200, wr["y"] + 200)
    pg.mouse.wheel(0, 28 * 500)
    time.sleep(0.3)
    top = ev(pg, "document.querySelector('#tablewrap').scrollTop")
    shown = ev(pg, "(() => { const w = document.querySelector('#tablewrap').getBoundingClientRect(); const el = document.elementFromPoint(w.left + 150, w.top + 100); const tr = el && el.closest('tr[data-id]'); return tr ? idx(tr.dataset.id) : -1; })()")
    check(top > 0 and abs(shown - round((top + 100 - 32) / 28)) <= 1, f"after scrolling the right rows show ({shown} at {top}px)")


def test_responsive(pg):
    fresh(pg)
    for w, cols in ((1280, 7), (720, 5), (420, 3)):
        pg.set_viewport_size({"width": w, "height": 760})
        time.sleep(0.3)
        n = ev(pg, "document.querySelectorAll('#thead th').length")
        check(n == cols, f"{w}px: {cols} columns, got {n}")
        check(ev(pg, "[...document.querySelectorAll('#tbody tr[data-id]')].every(tr => tr.children.length === document.querySelectorAll('#thead th').length)"), f"{w}px: rows match the columns")
        check(not ev(pg, "document.documentElement.scrollWidth > innerWidth"), f"{w}px: nothing overflows")
        focus_grid(pg)
        pg.keyboard.press("End")
        time.sleep(0.1)
        check(row_visible(pg, ev(pg, "'r-' + LIST.focus")), f"{w}px: End shows the last row")
    pg.set_viewport_size({"width": 1280, "height": 800})
    time.sleep(0.2)


def test_live_updates(pg):
    fresh(pg)
    tok = SECRET

    def api(path, body):
        req = urllib.request.Request(BASE + "/api/" + path, data=json.dumps(body).encode(), headers={"X-Token": tok, "Content-Type": "application/json"})
        return json.loads(urllib.request.urlopen(req).read())

    def server_list():
        """The whole list from a new stream: what the window should have."""
        return ev(pg, """() => new Promise(res => { const s = new EventSource('/api/events'); s.onmessage = e => { s.close(); res(JSON.parse(e.data).downloads); }; })""")

    def same():
        want = server_list()
        have = ev(pg, "S.downloads")
        check([d["id"] for d in have] == [d["id"] for d in want], "the window's list has the app's order")
        check(have == want, "…and the same rows")
        check(ev(pg, "S.downloads.every(d => S.byId.get(d.id) === d) && S.byId.size === S.downloads.length"), "S.byId matches")

    seq0 = ev(pg, "EV.seq")
    time.sleep(1)
    check(ev(pg, "EV.seq") >= seq0 + 2, "updates keep coming")
    ids = ev(pg, "S.downloads.slice(0, 6).map(d => d.id)")
    # changes, additions, removals and moves reach the window
    new = [api("add", {"url": f"https://example.com/new-{i}.zip", "name": f"new-{i}.zip"})["id"] for i in range(3)]
    api("action", {"action": "remove", "ids": ids[:2]})
    api("action", {"action": "up", "ids": [ids[4]]})
    time.sleep(1)
    same()
    check(ev(pg, f"listIds().includes('{new[0]}') && !S.byId.has('{ids[0]}')"), "new rows show, removed ones go")
    # while hidden: an update every 2 s, then back to normal at once
    ev(pg, "Object.defineProperty(document, 'hidden', { configurable: true, get: () => true }); document.dispatchEvent(new Event('visibilitychange'))")
    time.sleep(0.6)
    s0 = ev(pg, "EV.seq")
    time.sleep(3.1)
    n = ev(pg, "EV.seq") - s0
    check(1 <= n <= 2, f"hidden: {n} updates in 3 s")
    ev(pg, "Object.defineProperty(document, 'hidden', { configurable: true, get: () => false }); document.dispatchEvent(new Event('visibilitychange'))")
    time.sleep(0.3)
    s0 = ev(pg, "EV.seq")
    time.sleep(2)
    n = ev(pg, "EV.seq") - s0
    check(n >= 4, f"shown again: {n} updates in 2 s")
    # a missed update: the window asks for the whole list again and carries on
    ev(pg, "(() => { window.__full = 0; const o = es.onmessage; es.onmessage = e => { if (JSON.parse(e.data).full) window.__full++; o(e); }; EV.seq -= 3; })()")
    time.sleep(1.2)
    check(ev(pg, "window.__full") == 1 and not ev(pg, "EV.gap"), "after a gap: one full update")
    same()
    # segments only for the open progress window
    pid = ev(pg, "S.downloads.find(d => d.status === 'paused').id")
    check(ev(pg, "S.downloads.every(d => !d.segs)"), "no segments while no progress window is open")
    ev(pg, f"progressDialog('{pid}')")
    time.sleep(0.8)
    check(ev(pg, f"!!S.byId.get('{pid}').segs") and ev(pg, f"S.downloads.filter(d => d.segs).length") == 1, "its segments arrive, nobody else's")
    close_dialogs(pg)
    time.sleep(0.8)
    check(ev(pg, "S.downloads.every(d => !d.segs)"), "closed: no more segments")
    # a reconnect starts over with the whole list
    ev(pg, "(() => { window.__full = 0; es.close(); connect(); const o = es.onmessage; es.onmessage = e => { if (JSON.parse(e.data).full) window.__full++; o(e); }; })()")
    time.sleep(1)
    check(ev(pg, "window.__full") == 1, "a new connection: the whole list")
    same()
    # the rows on screen redraw only when their download changed
    ev(pg, "(() => { window.__fills = 0; const f = fillRow; fillRow = (tr, d, i) => { if (tr._d !== d) window.__fills++; f(tr, d, i); }; })()")
    time.sleep(1.5)
    check(ev(pg, "window.__fills") == 0, f"nothing changed, nothing redrawn ({ev(pg, 'window.__fills')})")


TESTS = [test_tab_order, test_menubar, test_context_menu, test_grid, test_headers, test_sidebar, test_shortcuts, test_mouse, test_responsive, test_live_updates]


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else None
    tmp = None
    if not binary:
        tmp = tempfile.mkdtemp(prefix="zl-bin-")
        binary = os.path.join(tmp, "zipline")
        subprocess.run(["go", "build", "-o", binary, "./cmd/zipline"], cwd=ROOT, check=True)
    app = App(binary)
    global SECRET
    SECRET = app.secret()
    failed = 0
    try:
        with sync_playwright() as p:
            b = p.chromium.launch()
            pg = b.new_page(viewport={"width": 1280, "height": 800})
            errors = []
            pg.on("pageerror", lambda e: errors.append(str(e)))
            pg.goto(app.login_url())
            pg.wait_for_selector("#tbody tr[data-id]")
            for t in TESTS:
                try:
                    t(pg)
                    print("ok  ", t.__name__)
                except Exception:
                    failed += 1
                    print("FAIL", t.__name__)
                    traceback.print_exc()
            if errors:
                failed += 1
                print("FAIL page errors:", errors)
            b.close()
    finally:
        app.stop()
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)
    print("FAIL" if failed else "PASS", f"({len(TESTS) - min(failed, len(TESTS))}/{len(TESTS)})")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
