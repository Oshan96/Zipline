#!/usr/bin/env python3
"""Checks web/colors.css against WCAG 2.2 contrast rules.

usage: python3 tools/contrast.py [-v]      (-v also lists the passing pairs; exit code 1 if any pair fails)

It reads the tokens of each theme from the /* @name */ blocks of colors.css (light, dark, and the
"more contrast" variants), then checks the pairs below on every surface they can appear on:
  text      4.5:1  (1.4.3)  body, muted, accent, ok/warn/err text, text on accent / danger fills
  non-text  3.0:1  (1.4.11) control borders, focus ring, selected-state borders, bars, accent fill
"""
import re, sys, os

HERE = os.path.dirname(os.path.abspath(__file__))
CSS = open(os.path.join(HERE, "..", "web", "colors.css")).read()

def lin(c):
    c /= 255
    return c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4

def lum(hexcol):
    h = hexcol.lstrip("#")
    r, g, b = (int(h[i:i + 2], 16) for i in (0, 2, 4))
    return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b)

def ratio(a, b):
    x, y = sorted((lum(a), lum(b)), reverse=True)
    return (x + 0.05) / (y + 0.05)

def block(tag):
    """the declarations of the rule that follows /* @tag */ (it may sit inside an @media block)"""
    m = re.search(r"/\*\s*@" + re.escape(tag) + r"\b[^*]*\*/", CSS)
    if not m:
        sys.exit(f"colors.css: no block tagged @{tag}")
    i = CSS.index("{", m.end())
    if CSS[m.end():i].strip().startswith("@media"):  # skip the @media's brace to the rule's own
        i = CSS.index("{", i + 1)
    body = CSS[i + 1:CSS.index("}", i)]
    body = re.sub(r"/\*.*?\*/", "", body, flags=re.S)
    return {k: v for k, v in re.findall(r"--([\w-]+)\s*:\s*([^;]+?)\s*(?:;|$)", body.strip())}

def resolve(tokens):
    out = {}
    for k, v in tokens.items():
        seen = 0
        while v.startswith("var(") and seen < 5:
            v = tokens[v[4:-1].strip()[2:]]
            seen += 1
        out[k] = v
    return out

light = resolve(block("light"))
dark = resolve({**block("light"), **block("dark")})  # (the dark block only restates what changes)
if block("dark") != block("dark-chosen"):
    sys.exit("colors.css: the @dark and @dark-chosen blocks differ")
light_more = resolve({**block("light"), **block("light-more")})
dark_more = resolve({**block("light"), **block("dark"), **block("dark-more")})
if block("dark-more") != block("dark-more-chosen"):
    sys.exit("colors.css: the @dark-more and @dark-more-chosen blocks differ")

SURF = ["bg", "panel", "panel2", "hover", "sel", "accent2"]           # where text and borders sit
PAIRS = []  # (what, foreground token, [background tokens], minimum ratio)
for fg, what in (("text", "body text"), ("muted", "muted text"), ("accent", "accent text (links, badges)"), ("ok", "green text"), ("warn", "amber text"), ("err", "red text")):
    PAIRS.append((what, fg, SURF, 4.5))
PAIRS += [
    ("text on accent fill (primary button, menu hover)", "accentText", ["accent"], 4.5),
    ("text on danger fill (Delete button)", "dangerText", ["danger"], 4.5),
    ("control border (input, select, button)", "ctl", SURF, 3.0),
    ("focus ring", "focus", SURF, 3.0),
    ("selected-state border", "selLine", ["panel", "panel2"], 3.0),
    ("accent fill (primary button) against the page", "accent", ["bg", "panel", "panel2"], 3.0),
    ("progress bar fill", "bar", ["barBg"], 3.0),
    ("progress fill, finished", "segDone", ["barBg"], 3.0),
    ("progress fill, paused", "warn", ["barBg"], 3.0),
    ("progress fill, failed", "err", ["barBg"], 3.0),
    ("segment map fill", "seg", ["barBg"], 3.0),
]

bad = 0
verbose = "-v" in sys.argv
themes = [("light", light), ("dark", dark), ("light, more contrast", light_more), ("dark, more contrast", dark_more)]
header = False
for what, fg, bgs, need in PAIRS:
    for tname, t in themes:
        rs = [(ratio(t[fg], t[b]), b) for b in bgs]
        worst, wb = min(rs)
        ok = worst >= need
        bad += not ok
        if verbose or not ok:
            if not header:
                header = True
                print(f"{'pair':52} {'theme':22} {'worst':>6}  need  on")
            print(f"{what + ' (' + t[fg] + ')':52} {tname:22} {worst:6.2f}  {need:>4}  {wb}{'' if ok else '   <-- FAIL'}")
# the table people read: each token against the panel (and the worst surface)
print("\ntoken table (value, ratio on panel / on the worst surface it must clear)")
for tok, bgs, need in [("text", SURF, 4.5), ("muted", SURF, 4.5), ("accent", SURF, 4.5), ("ok", SURF, 4.5), ("warn", SURF, 4.5), ("err", SURF, 4.5),
                       ("ctl", SURF, 3.0), ("focus", SURF, 3.0), ("selLine", ["panel", "panel2"], 3.0)]:
    row = []
    for tname, t in themes[:2]:
        row.append(f"{t[tok]} {ratio(t[tok], t['panel']):.2f}/{min(ratio(t[tok], t[b]) for b in bgs):.2f}")
    print(f"  --{tok:8} light {row[0]:22} dark {row[1]}")
for tname, t in themes[:2]:
    print(f"  on-accent text, {tname}: {t['accentText']} on {t['accent']} = {ratio(t['accentText'], t['accent']):.2f}")
print("\nFAILED" if bad else f"\nall {len(PAIRS) * len(themes)} pair checks pass (text 4.5:1, non-text 3:1)")
sys.exit(1 if bad else 0)
