#!/usr/bin/env python3
"""Build the README's illustrations into docs/assets/readme/.

Every glyph is outlined from IBM Plex (the wordmark's family, SIL OFL), so the
SVGs render identically on any machine and terminal columns stay aligned.
Fonts are downloaded once into a cache directory; nothing here runs in CI.

    python3 scripts/readme-art/build.py
"""

import os
import pathlib
import urllib.request

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer

ROOT = pathlib.Path(__file__).resolve().parents[2]
OUT = pathlib.Path(os.environ.get("README_ART_OUT", ROOT / "docs/assets/readme"))
CACHE = pathlib.Path(os.environ.get("README_ART_FONTS", pathlib.Path.home() / ".cache/spotexec-readme-fonts"))
FONTS = {
    "mono": "ibmplexmono/IBMPlexMono-Regular.ttf",
    "mono-medium": "ibmplexmono/IBMPlexMono-Medium.ttf",
    "mono-semibold": "ibmplexmono/IBMPlexMono-SemiBold.ttf",
    "sans-vf": "ibmplexsans/IBMPlexSans%5Bwdth,wght%5D.ttf",
}

INK, SPOT = "#0B0D10", "#FF7300"


# ---------------------------------------------------------------- type setting

class Face:
    def __init__(self, font: TTFont):
        self.font = font
        self.cmap = font.getBestCmap()
        self.glyphs = font.getGlyphSet()
        self.upm = font["head"].unitsPerEm
        self.kern = {}
        try:  # pair kerning from GPOS PairPos format 1, enough for Plex Sans
            for lookup in font["GPOS"].table.LookupList.Lookup:
                for st in lookup.SubTable:
                    st = getattr(st, "ExtSubTable", st)
                    if getattr(st, "Format", None) == 1 and st.LookupType == 2:
                        for i, first in enumerate(st.Coverage.glyphs):
                            for pvr in st.PairSet[i].PairValueRecord:
                                v = pvr.Value1
                                if v is not None and getattr(v, "XAdvance", 0):
                                    self.kern.setdefault((first, pvr.SecondGlyph), v.XAdvance)
                    elif getattr(st, "Format", None) == 2 and st.LookupType == 2:
                        c1, c2 = st.ClassDef1.classDefs, st.ClassDef2.classDefs
                        for first in st.Coverage.glyphs:
                            row = st.Class1Record[c1.get(first, 0)]
                            for second, k2 in list(c2.items()):
                                v = row.Class2Record[k2].Value1
                                if v is not None and getattr(v, "XAdvance", 0):
                                    self.kern.setdefault((first, second), v.XAdvance)
        except KeyError:
            pass

    def glyph(self, ch):
        return self.cmap.get(ord(ch)) or self.cmap[ord("?")]

    def width(self, s, size, track=0.0):
        names = [self.glyph(c) for c in s]
        w = 0
        for i, g in enumerate(names):
            w += self.font["hmtx"][g][0] + track * self.upm
            if i + 1 < len(names):
                w += self.kern.get((g, names[i + 1]), 0)
        return w * size / self.upm

    def layout(self, s, track=0.0):
        """Glyph names and their x offsets in font units."""
        out, cx = [], 0.0
        names = [self.glyph(c) for c in s]
        for i, g in enumerate(names):
            if s[i] != " ":
                out.append((g, cx))
            cx += self.font["hmtx"][g][0] + track * self.upm
            if i + 1 < len(names):
                cx += self.kern.get((g, names[i + 1]), 0)
        return out

    def outline(self, g):
        pen = SVGPathPen(self.glyphs, ntos=lambda v: "%d" % round(v))
        self.glyphs[g].draw(pen)
        return pen.getCommands()


# Glyphs are drawn once per SVG in <defs> and placed with <use>.
DEFS = {}


def glyph_ref(face, g):
    key = (face, g)
    if key not in DEFS:
        DEFS[key] = (f"g{len(DEFS)}", F[face].outline(g))
    return DEFS[key][0]


def load_faces():
    CACHE.mkdir(parents=True, exist_ok=True)
    paths = {}
    for key, rel in FONTS.items():
        p = CACHE / pathlib.Path(rel).name.replace("%5B", "[").replace("%5D", "]")
        if not p.exists():
            urllib.request.urlretrieve(f"https://github.com/google/fonts/raw/main/ofl/{rel}", p)
        paths[key] = p
    faces = {k: Face(TTFont(paths[k])) for k in ("mono", "mono-medium", "mono-semibold")}
    for name, wght in (("sans", 400), ("sans-medium", 500), ("sans-semibold", 600)):
        vf = TTFont(paths["sans-vf"])
        faces[name] = Face(instancer.instantiateVariableFont(vf, {"wght": wght, "wdth": 100}))
    return faces


F = None


def text(s, x, y, size, face="sans", fill="#000", anchor="start", track=0.0):
    f = F[face]
    if anchor != "start":
        w = f.width(s, size, track)
        x -= w if anchor == "end" else w / 2
    k = size / f.upm
    uses = "".join(f'<use href="#{glyph_ref(face, g)}" x="{round(cx)}"/>' for g, cx in f.layout(s, track))
    if not uses:
        return ""
    return f'<g fill="{fill}" transform="translate({x:.2f} {y:.2f}) scale({k:.5f} -{k:.5f})">{uses}</g>'


def svg(w, h, body, title, desc):
    defs = "".join(f'<path id="{i}" d="{d}"/>' for i, d in DEFS.values() if d)
    DEFS.clear()
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" role="img" aria-labelledby="t d">'
        f'<title id="t">{title}</title><desc id="d">{desc}</desc><defs>{defs}</defs>{body}</svg>\n'
    )


def write(name, content):
    OUT.mkdir(parents=True, exist_ok=True)
    (OUT / name).write_text(content)
    print(OUT / name)


# ---------------------------------------------------------------- themes

PAGE = {
    "light": {"fg": "#1F2328", "muted": "#59636E", "line": "#D1D9E0", "panel": "#F6F8FA", "card": "#FFFFFF", "edge": "#D1D9E0"},
    "dark": {"fg": "#F0F6FC", "muted": "#9198A1", "line": "#3D444D", "panel": "#151B23", "card": "#0D1117", "edge": "#3D444D"},
}

# ---------------------------------------------------------------- terminal

TERM = {"bg": "#0B0D10", "bar": "#15181D", "edge": "#2B3038", "fg": "#E6E8EB", "dim": "#8B929B", "green": "#3FB950"}


def check(x, y, size, fill):
    """A check mark drawn to sit on the mono baseline like vitest's."""
    s = size
    return (f'<path fill="none" stroke="{fill}" stroke-width="{s*0.13:.2f}" stroke-linecap="round" stroke-linejoin="round" '
            f'd="M{x+s*0.08:.2f} {y-s*0.34:.2f} L{x+s*0.24:.2f} {y-s*0.16:.2f} L{x+s*0.54:.2f} {y-s*0.6:.2f}"/>')


def terminal(theme):
    """The hero: one real run, sized to its content so it stays legible on phones."""
    P = PAGE[theme]
    T = TERM
    size, lh, pad, bar = 15, 26, 28, 38
    lines = [
        [("~/code/my-app", T["dim"]), (" $ ", T["dim"]), ("sx", SPOT), (" pnpm vitest run -u src/auth.test.ts", T["fg"])],
        [],
        [(" ", ""), ("✓", "check"), (" src/auth.test.ts ", T["fg"]), ("(12 tests) 340ms", T["dim"])],
        [],
        [(" Snapshots  ", T["dim"]), ("2 updated", T["fg"])],
        [("     Tests  ", T["dim"]), ("12 passed", T["green"]), (" (12)", T["dim"])],
        [],
        [("●", "dot"), (" 4.1s on aws#3 · synced 4 files (18 KB) · pulled 2 modified files", T["dim"])],
    ]
    adv = F["mono"].width("0", size)
    longest = max(sum(len(t) for t, _ in runs) for runs in lines)
    W = round(pad * 2 + longest * adv + 10)
    TH = bar + pad + lh * (len(lines) - 1) + pad + 6
    ox, oy = 10, 4
    body = [
        f'<defs><filter id="s" x="-5%" y="-5%" width="110%" height="125%"><feDropShadow dx="0" dy="5" stdDeviation="8" flood-color="#0B0D10" flood-opacity="{".16" if theme == "light" else "0"}"/></filter>'
        f'<clipPath id="c"><rect x="{ox}" y="{oy}" width="{W}" height="{TH}" rx="12"/></clipPath></defs>',
        f'<rect x="{ox}" y="{oy}" width="{W}" height="{TH}" rx="12" fill="{T["bg"]}" filter="url(#s)"/>',
        f'<g clip-path="url(#c)"><rect x="{ox}" y="{oy}" width="{W}" height="{bar}" fill="{T["bar"]}"/></g>',
        f'<rect x="{ox+.5}" y="{oy+.5}" width="{W-1}" height="{TH-1}" rx="11.5" fill="none" stroke="{T["edge"] if theme == "light" else "#3D444D"}"/>',
        f'<line x1="{ox}" x2="{ox+W}" y1="{oy+bar}" y2="{oy+bar}" stroke="{T["edge"]}"/>',
    ]
    for i in range(3):
        body.append(f'<circle cx="{ox + 22 + i*19}" cy="{oy + bar/2}" r="5.5" fill="#30353D"/>')
    body.append(text("my-app · zsh", ox + W / 2, oy + bar / 2 + 4.5, 12.5, "sans-medium", T["dim"], "middle"))
    x0, y = ox + pad, oy + bar + pad + 8
    for runs in lines:
        col = 0
        for s_, fill in runs:
            cx = x0 + col * adv
            if fill == "dot":
                body.append(f'<circle cx="{cx + adv/2:.2f}" cy="{y - size*0.33:.2f}" r="{size*0.3:.2f}" fill="{T["green"]}"/>')
            elif fill == "check":
                body.append(check(cx, y, size * 1.15, T["green"]))
            elif s_.strip():
                body.append(text(s_, cx, y, size, "mono-semibold" if fill == SPOT else "mono", fill))
            col += len(s_)
        y += lh
    # Brackets under the summary line's three segments, labelled on the page below the frame.
    summary = "● 4.1s on aws#3 · synced 4 files (18 KB) · pulled 2 modified files"
    segs = [("4.1s on aws#3", "ran on a Spot VM"), ("synced 4 files (18 KB)", "sent only what changed"), ("pulled 2 modified files", "wrote the snapshots back")]
    ay = oy + TH + 16
    for seg, label in segs:
        i = summary.index(seg)
        x1, x2 = x0 + i * adv, x0 + (i + len(seg)) * adv
        mid = (x1 + x2) / 2
        body.append(f'<path fill="none" stroke="{P["muted"]}" stroke-opacity=".75" stroke-width="1.5" d="M{x1:.1f} {ay-7} V{ay} H{x2:.1f} V{ay-7} M{mid:.1f} {ay} V{ay+7}"/>')
        body.append(text(label, mid, ay + 26, 14, "sans-medium", P["muted"], "middle"))
    H = ay + 40
    write(f"terminal-{theme}.svg", svg(W + 20, H, "".join(body),
        "sx pnpm vitest run -u src/auth.test.ts",
        "A terminal running sx pnpm vitest run -u src/auth.test.ts. Vitest's output streams back as if it ran locally: "
        "src/auth.test.ts, 12 tests passed, 2 snapshots updated. spotexec then prints one summary line: "
        "4.1s on aws#3 (ran on a Spot VM), synced 4 files (18 KB) (sent only what changed), "
        "pulled 2 modified files (wrote the snapshots back)."))


# ---------------------------------------------------------------- how it works

def rrect(x, y, w, h, r, fill="none", stroke="none", dash=False, sw=1):
    d = ' stroke-dasharray="5 4"' if dash else ""
    return f'<rect x="{x+sw/2:.1f}" y="{y+sw/2:.1f}" width="{w-sw:.1f}" height="{h-sw:.1f}" rx="{r}" fill="{fill}" stroke="{stroke}" stroke-width="{sw}"{d}/>'


def arrow(x1, x2, y, color, heads=("right",)):
    a = 7
    d = f"M{x1+1} {y} H{x2-1}"
    if "right" in heads:
        d += f" M{x2-a} {y-a/1.4:.1f} L{x2} {y} L{x2-a} {y+a/1.4:.1f}"
    if "left" in heads:
        d += f" M{x1+a} {y-a/1.4:.1f} L{x1} {y} L{x1+a} {y+a/1.4:.1f}"
    return f'<path fill="none" stroke="{color}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" d="{d}"/>'


def slots(x, y, busy, total, P):
    out = []
    for i in range(total):
        sx = x + i * 17
        if i < busy:
            out.append(f'<rect x="{sx}" y="{y}" width="12" height="12" rx="3" fill="{SPOT}"/>')
        else:
            out.append(rrect(sx, y, 12, 12, 3, "none", P["muted"], sw=1.2))
    return "".join(out)


def how(theme):
    """Three worktrees, three commands, three busy slots: the homelab fills first."""
    P = PAGE[theme]
    # Orange text on white is 2.7:1, so on light cards `sx` is set in the text colour, weight carrying it.
    sx_fill = SPOT if theme == "dark" else P["fg"]
    W, H = 760, 352
    pw = 270
    b = []
    cap = lambda s_, x, y: text(s_.upper(), x, y, 11.5, "sans-semibold", P["muted"], track=0.08)

    # Left: this machine, three worktrees, one command each.
    lx = 0
    b.append(rrect(lx, 0, pw, H, 14, P["panel"], P["edge"]))
    b.append(cap("Your machine", lx + 20, 34))
    wts = [("agent-1", "feat/auth", "pnpm test"), ("agent-2", "fix/types", "pnpm tsc --noEmit"), ("agent-3", "chore/fmt", "pnpm prettier --write .")]
    for i, (name, br, cmd) in enumerate(wts):
        cy = 52 + i * 80
        b.append(rrect(lx + 14, cy, pw - 28, 68, 10, P["card"], P["edge"]))
        b.append(text(name, lx + 28, cy + 27, 15, "sans-semibold", P["fg"]))
        b.append(text(br, lx + pw - 28, cy + 27, 13, "mono", P["muted"], "end"))
        b.append(text("sx", lx + 28, cy + 51, 13, "mono-semibold", sx_fill))
        b.append(text(cmd, lx + 28 + F["mono"].width("sx ", 13), cy + 51, 13, "mono", P["fg"]))
    b.append(text("git worktrees, a fair local queue,", lx + 20, H - 38, 13.5, "sans", P["muted"]))
    b.append(text("nothing hosted, no control plane", lx + 20, H - 17, 13.5, "sans", P["muted"]))

    # Right: runners. Three commands fill three slots: the homelab first, then a Spot VM.
    rx = W - pw
    b.append(rrect(rx, 0, pw, H, 14, P["panel"], P["edge"]))
    b.append(cap("Your runners", rx + 20, 34))
    vms = [("homelab-01", "ssh", 2, False, "2 of 2 slots busy"), ("aws#1", "spot", 1, False, "1 of 2 slots busy"), ("aws#2", "spot", 0, True, "idle, powering off")]
    for i, (name, kind, busy, dash, note) in enumerate(vms):
        cy = 52 + i * 80
        b.append(rrect(rx + 14, cy, pw - 28, 68, 10, "none" if dash else P["card"], P["edge"], dash=dash))
        b.append(text(name, rx + 28, cy + 27, 15, "sans-semibold", P["muted"] if dash else P["fg"]))
        b.append(text(kind, rx + 28 + F["sans-semibold"].width(name, 15) + 8, cy + 27, 13, "mono", P["muted"]))
        b.append(slots(rx + pw - 28 - (2 * 17 - 5), cy + 16, busy, 2, P))
        b.append(text(note, rx + 28, cy + 51, 13.5, "sans", P["muted"]))
    b.append(text("Spot VMs launch for queued work", rx + 20, H - 38, 13.5, "sans", P["muted"]))
    b.append(text("and power themselves off when idle", rx + 20, H - 17, 13.5, "sans", P["muted"]))

    # Middle: the three steps over SSH.
    mx1, mx2 = pw + 8, rx - 8
    mid = (mx1 + mx2) / 2
    steps = [
        (88, ("right",), "1  sync", "only missing files"),
        (168, ("right", "left"), "2  run", "output, exit code back"),
        (248, ("left",), "3  write back", "3-way checked"),
    ]
    for y, heads, label, sub in steps:
        b.append(text(label, mid, y - 13, 15, "sans-semibold", P["fg"], "middle"))
        b.append(arrow(mx1, mx2, y, P["muted"], heads))
        b.append(text(sub, mid, y + 23, 13.5, "sans", P["muted"], "middle"))
    b.append(rrect(mid - 70, H - 50, 140, 30, 15, "none", P["edge"]))
    b.append(text("SSH · your keys", mid, H - 30, 13, "mono", P["muted"], "middle"))

    write(f"how-it-works-{theme}.svg", svg(W, H, "".join(b),
        "How spotexec works",
        "Left: your machine, with three git worktrees (agent-1, agent-2, agent-3) running sx pnpm test, sx pnpm tsc --noEmit and "
        "sx pnpm prettier --write .; a fair local queue, nothing hosted and no control plane. Middle, over SSH with your own keys: "
        "1 sync, only the files the slot is missing; 2 run, output and exit code stream back; 3 write back, 3-way checked. "
        "Right: your runners. homelab-01 over SSH has both slots busy, Spot VM aws#1 has one of two busy, and aws#2 is idle and "
        "powering off. Spot VMs launch for queued work and power themselves off when idle."))


# ---------------------------------------------------------------- lockups

def lockups():
    """The README hero: the settled lockup, with a hairline on the dark tile.

    The drawing is copied from docs/assets/logo unchanged. On dark canvases the
    tile becomes brand ink with a 1px-at-display edge, so it holds on GitHub dark
    (#0d1117) and dark dimmed (#212830) alike; #252A33 all but vanishes on dimmed.
    """
    import re
    logo = ROOT / "docs/assets/logo"
    write("logo-light.svg", (logo / "spotexec.svg").read_text())
    dark = (logo / "spotexec-dark.svg").read_text()
    tile = re.search(r'<path fill="#252A33" d="([^"]+)"/>', dark).group(1)
    edge = f'<path fill="none" stroke="#3D444D" stroke-width="4" transform="translate(2 2) scale(0.984375)" d="{tile}"/>'
    write("logo-dark.svg", dark.replace(f'<path fill="#252A33" d="{tile}"/>', f'<path fill="{INK}" d="{tile}"/>{edge}'))


def main():
    global F
    F = load_faces()
    lockups()
    for theme in ("light", "dark"):
        terminal(theme)
        how(theme)


if __name__ == "__main__":
    main()
