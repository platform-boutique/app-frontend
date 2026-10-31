#!/usr/bin/env python3
"""Contrast check for the colour tokens in static/styles/styles.css (stdlib only).

Reads the light tokens (:root), the OS-dark block and the explicit dark block
(:root[data-theme="dark"]), checks that the two dark blocks agree, computes the
WCAG 2.x contrast ratio of the pairs that matter and exits 1 if any fails.

    python3 tools/check-contrast.py [path/to/styles.css]

Thresholds: 4.5 for text, 3.0 for non-text (WCAG 1.4.11: control borders,
graphics such as the stars and the logo).
"""
import colorsys
import pathlib
import re
import sys

TEXT, UI = 4.5, 3.0

# (foreground, background, minimum, what it is for)
PAIRS = [
    ("text", "bg", TEXT, "body text on the page"),
    ("text", "surface", TEXT, "text on cards"),
    ("text", "surface-2", TEXT, "text on the secondary surface"),
    ("text-muted", "bg", TEXT, "secondary text on the page"),
    ("text-muted", "surface", TEXT, "secondary text on cards"),
    ("text-muted", "surface-2", TEXT, "secondary text on the secondary surface"),
    ("bg", "text", TEXT, "announcement bar (page colour on text colour)"),
    ("accent", "bg", TEXT, "links on the page"),
    ("accent", "surface", TEXT, "links on cards"),
    ("accent-hover", "bg", TEXT, "link hover on the page"),
    ("accent-contrast", "accent", TEXT, "primary button label"),
    ("accent-contrast", "accent-hover", TEXT, "primary button label, hover"),
    ("text", "accent-soft", TEXT, "text on the hero and soft badges"),
    ("accent", "accent-soft", TEXT, "accent text on soft badges"),
    ("sage-strong", "sage", TEXT, "badge and ad label text on sage"),
    ("sage-strong", "surface", TEXT, "eyebrow and perk icons on cards"),
    ("text", "sage", TEXT, "ad card link on sage"),
    ("danger", "danger-soft", TEXT, "error text on its soft background"),
    ("danger", "bg", TEXT, "error text on the page"),
    ("danger", "surface", TEXT, "error text on cards"),
    ("success", "success-soft", TEXT, "success text on its soft background"),
    ("success", "surface", TEXT, "success text on cards"),
    ("control-border", "bg", UI, "input and button borders on the page"),
    ("control-border", "surface", UI, "input and button borders on cards"),
    ("control-border", "surface-2", UI, "input and button borders on the secondary surface"),
    ("focus", "bg", UI, "focus ring on the page"),
    ("focus", "surface", UI, "focus ring on cards"),
    ("accent", "bg", UI, "logo on the page"),
    ("brass", "surface", UI, "stars and brass graphics on cards"),
    ("brass", "bg", UI, "stars and brass graphics on the page"),
]

# success must not read as the brand accent: at least this many degrees of hue apart
DISTINCT = [("success", "accent", 30)]


def blocks(css):
    """Return {theme: {token: hex}} for light, dark-os and dark-manual."""
    def tokens(body):
        return {k: v.lower() for k, v in re.findall(r"--color-([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\s*;", body)}

    light = re.search(r":root\s*\{(.*?)\n\}", css, re.S)
    dark_os = re.search(r"prefers-color-scheme:\s*dark\)\s*\{\s*:root:not\(\[data-theme=\"light\"\]\)\s*\{(.*?)\n  \}", css, re.S)
    dark_manual = re.search(r":root\[data-theme=\"dark\"\]\s*\{(.*?)\n\}", css, re.S)
    if not (light and dark_os and dark_manual):
        sys.exit("check-contrast: cannot find the :root, prefers-color-scheme: dark and data-theme=dark blocks")
    return tokens(light.group(1)), tokens(dark_os.group(1)), tokens(dark_manual.group(1))


def lum(h):
    c = [int(h[i:i + 2], 16) / 255 for i in (1, 3, 5)]
    c = [x / 12.92 if x <= 0.03928 else ((x + 0.055) / 1.055) ** 2.4 for x in c]
    return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]


def hue(h):
    return colorsys.rgb_to_hls(*(int(h[i:i + 2], 16) / 255 for i in (1, 3, 5)))[0] * 360


def ratio(a, b):
    la, lb = sorted((lum(a), lum(b)), reverse=True)
    return (la + 0.05) / (lb + 0.05)


def main():
    path = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parent.parent / "static/styles/styles.css")
    light, dark_os, dark_manual = blocks(path.read_text())
    failed = 0
    if dark_os != dark_manual:
        diff = sorted(k for k in set(dark_os) | set(dark_manual) if dark_os.get(k) != dark_manual.get(k))
        print(f"FAIL the two dark blocks differ in: {', '.join(diff)}")
        failed += 1
    for name, theme in (("light", light), ("dark", dark_manual)):
        print(f"\n{name} theme")
        print(f"  {'pair':<34}{'ratio':>7}  {'min':>4}  result  use")
        for fg, bg, minimum, use in PAIRS:
            if fg not in theme or bg not in theme:
                print(f"  {fg + ' on ' + bg:<34}  missing token")
                failed += 1
                continue
            r = ratio(theme[fg], theme[bg])
            ok = r >= minimum
            failed += not ok
            print(f"  {fg + ' on ' + bg:<34}{r:7.2f}  {minimum:4.1f}  {'ok  ' if ok else 'FAIL'}    {use}")
        for a, b, degrees in DISTINCT:
            d = abs(hue(theme[a]) - hue(theme[b]))
            d = min(d, 360 - d)
            ok = d >= degrees
            failed += not ok
            print(f"  {a + ' vs ' + b + ' (hue)':<34}{d:6.0f}d  {degrees:3d}d  {'ok  ' if ok else 'FAIL'}    must not look like the brand accent")
    print(f"\n{failed} failure(s)" if failed else "\nall pairs pass")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
