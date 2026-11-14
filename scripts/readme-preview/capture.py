#!/usr/bin/env python3
"""Render README.md the way github.com does and screenshot it.

Opt-in design harness, never run in CI. It sends README.md through GitHub's
own Markdown API (the same sanitizer and syntax highlighter the repository
page uses), wraps the result in GitHub's markdown stylesheet inside a frame
the width of the repository page's README box, and captures it in GitHub's
light, dark and dark dimmed themes with Playwright.

    DESIGN_CAPTURE_DIR=/tmp/readme-shots python3 scripts/readme-preview/capture.py

Needs: an authenticated `gh`, Python Playwright with Chromium, network access.
Every capture is checked before it is written: the README must have rendered,
every image must have loaded, and the theme must be the one asked for.
"""

import asyncio
import json
import re
import os
import pathlib
import subprocess
import sys
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
REPO = "daintreehq/spotexec"
CSS_URL = "https://cdn.jsdelivr.net/npm/github-markdown-css@5/github-markdown.css"

# GitHub's three canvas themes. Dark dimmed has no stylesheet of its own in
# github-markdown-css, so it is the dark sheet with Primer's dimmed tokens.
THEMES = {
    "light": {"scheme": "light", "canvas": "#ffffff", "page": "#ffffff", "border": "#d1d9e0", "fg": "#1f2328", "muted": "#59636e", "tab": "#fd8c73"},
    "dark": {"scheme": "dark", "canvas": "#0d1117", "page": "#0d1117", "border": "#3d444d", "fg": "#f0f6fc", "muted": "#9198a1", "tab": "#f78166"},
    "dimmed": {"scheme": "dark", "canvas": "#212830", "page": "#212830", "border": "#3d444d", "fg": "#d1d7e0", "muted": "#9198a1", "tab": "#ec775c"},
}
DIMMED_VARS = """
.markdown-body {
  --bgColor-default: #212830; --bgColor-muted: #262c36; --bgColor-neutral-muted: #656c7633;
  --bgColor-attention-muted: #ae7c1426; --borderColor-default: #3d444d; --borderColor-muted: #3d444db3;
  --borderColor-neutral-muted: #3d444db3; --fgColor-default: #d1d7e0; --fgColor-muted: #9198a1;
  --fgColor-accent: #478be6; --focus-outlineColor: #316dca;
}
"""

ARTICLE_WIDTH = 838  # the README article on a 1280px-wide repository page
VIEWPORTS = {"desktop": (1280, 900), "mobile": (390, 844)}
SLICE = 1400
OVERLAP = 120  # slices overlap so no line is cut in half at a seam


def render_markdown() -> str:
    text = (ROOT / "README.md").read_text()
    out = subprocess.run(
        ["gh", "api", "markdown", "-f", "mode=markdown", "-f", f"context={REPO}", "-f", f"text={text}"],
        capture_output=True, text=True, check=True,
    )
    if "<" not in out.stdout:
        raise SystemExit("GitHub's Markdown API returned no HTML")
    # The API wraps every <img> in a link to the file. On the repository page the
    # <img> inside a <picture> is a direct child (that is what lets the browser pick
    # the <source> for the theme), so unwrap it here the same way.
    return re.sub(r"(<picture>(?:(?!</picture>).)*?)<a [^>]*>(<img [^>]*>)</a>", r"\1\2", out.stdout, flags=re.S)


def stylesheet(cache: pathlib.Path) -> str:
    path = cache / "github-markdown.css"
    if not path.exists():
        cache.mkdir(parents=True, exist_ok=True)
        urllib.request.urlretrieve(CSS_URL, path)
    return path.read_text()


def page(html: str, css: str, theme: str, width: int) -> str:
    t = THEMES[theme]
    extra = DIMMED_VARS if theme == "dimmed" else ""
    return f"""<!doctype html><html data-theme="{theme}"><head><meta charset="utf-8">
<base href="{ROOT.as_uri()}/">
<style>{css}{extra}
html,body{{margin:0;background:{t['page']};}}
body{{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans",Helvetica,Arial,sans-serif;}}
#box{{width:{width}px;margin:16px auto 40px;border:1px solid {t['border']};border-radius:6px;background:{t['canvas']};}}
#tabs{{display:flex;gap:24px;align-items:center;height:48px;padding:0 16px;border-bottom:1px solid {t['border']};font-size:14px;color:{t['muted']};}}
#tabs .on{{color:{t['fg']};font-weight:600;box-shadow:inset 0 -2px 0 {t['tab']};height:48px;display:flex;align-items:center;}}
.markdown-body{{box-sizing:border-box;padding:32px;background:{t['canvas']};}}
@media (max-width:600px){{.markdown-body{{padding:16px;}}}}
</style></head><body><div id="box"><div id="tabs"><span class="on">README</span><span>Contributing</span><span>MIT license</span></div>
<article class="markdown-body">{html}</article></div></body></html>"""


async def capture(out: pathlib.Path) -> list[str]:
    from playwright.async_api import async_playwright

    html = render_markdown()
    css = stylesheet(out / ".cache")
    written: list[str] = []
    async with async_playwright() as p:
        browser = await p.chromium.launch()
        for theme, t in THEMES.items():
            for vp, (vw, vh) in VIEWPORTS.items():
                width = min(ARTICLE_WIDTH + 2, vw - 16)
                ctx = await browser.new_context(viewport={"width": vw, "height": vh}, color_scheme=t["scheme"], device_scale_factor=2)
                pg = await ctx.new_page()
                src = out / f".page-{theme}-{vp}.html"
                src.write_text(page(html, css, theme, width))
                await pg.goto(src.as_uri(), wait_until="networkidle")
                await pg.wait_for_timeout(300)
                check = await pg.evaluate(
                    """() => {
                      const imgs = [...document.querySelectorAll('.markdown-body img')];
                      const broken = imgs.filter(i => !i.complete || i.naturalWidth === 0).map(i => i.getAttribute('src'));
                      const scheme = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
                      const wrongTheme = [...document.querySelectorAll('.markdown-body picture')].filter(pc => {
                        const want = [...pc.querySelectorAll('source')].find(s => matchMedia(s.media).matches);
                        const img = pc.querySelector('img');
                        return want && !img.currentSrc.endsWith(want.getAttribute('srcset'));
                      }).map(pc => pc.querySelector('img').getAttribute('src'));
                      return {images: imgs.length, broken, wrongTheme, scheme, text: document.querySelector('.markdown-body').innerText.length,
                              height: document.querySelector('#box').getBoundingClientRect().bottom + 40};
                    }"""
                )
                if check["broken"]:
                    raise SystemExit(f"{theme}/{vp}: images failed to load: {check['broken']}")
                if check["wrongTheme"]:
                    raise SystemExit(f"{theme}/{vp}: <picture> showed the wrong theme's image: {check['wrongTheme']}")
                if check["scheme"] != t["scheme"] or check["text"] < 200:
                    raise SystemExit(f"{theme}/{vp}: render check failed: {json.dumps(check)}")
                name = f"top-{vp}--{theme}.png"
                await pg.screenshot(path=str(out / name))
                written.append(name)
                if vp == "desktop":
                    total = int(check["height"])
                    await pg.set_viewport_size({"width": vw, "height": total})
                    await pg.wait_for_timeout(150)
                    for i, y in enumerate(range(0, total - OVERLAP, SLICE - OVERLAP), start=1):
                        name = f"full-{i:02d}--{theme}.png"
                        await pg.screenshot(path=str(out / name), clip={"x": 0, "y": y, "width": vw, "height": min(SLICE, total - y)})
                        written.append(name)
                    name = f"overview--{theme}.png"
                    await pg.screenshot(path=str(out / name), full_page=True)
                    written.append(name)
                await ctx.close()
        await browser.close()
    return written


def main() -> None:
    out = os.environ.get("DESIGN_CAPTURE_DIR")
    if not out:
        raise SystemExit("set DESIGN_CAPTURE_DIR to an empty directory outside the repository")
    out_dir = pathlib.Path(out).resolve()
    if ROOT in out_dir.parents or out_dir == ROOT:
        raise SystemExit("DESIGN_CAPTURE_DIR must be outside the repository")
    out_dir.mkdir(parents=True, exist_ok=True)
    written = asyncio.run(capture(out_dir))
    missing = [n for n in written if not (out_dir / n).is_file() or (out_dir / n).stat().st_size == 0]
    if missing:
        raise SystemExit(f"captures not written: {missing}")
    for n in written:
        print(out_dir / n)


if __name__ == "__main__":
    sys.exit(main())
