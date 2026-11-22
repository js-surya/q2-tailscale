"""Tailscale-style icons. The status bar's: a 3x3 grid of dots (Tailscale Android's ic_notification geometry: viewport 100,
dots of radius 12.5 at 12.5/50/87.5), white, lit dots opaque and the others at 40%. Drawn 4x and scaled down.
bar-warn.png is the connected icon with a red dot over its top-right one: a warning (Tailscale's health, or DNS), as
Tailscale Android marks its icon. The row icons (icon.png 52 px, icon-40.png 40 px): the connected dots on the app's dark tile, cut round as
Settings → Network's other rows; icon-minimal.png and icon-40-minimal.png, Minimal's: on its grey discs.
usage: python3 bar_icons.py OUT_DIR"""
import sys
from PIL import Image, ImageDraw

out = sys.argv[1]
ON = {(0, 1), (1, 1), (2, 1), (1, 2)}  # (column, row) lit when connected: the middle row and the bottom middle


# The status bar's: as tall as stock's Wi-Fi bars (bar_wifi4.png: 14x16, ink on rows 3-13), the grid
# GRID px square on those rows, in a GRID x 16 image, so it sits level with Wi-Fi and the battery.
GRID, TOP, H = 11, 3, 16


def icon(lit, warn=False):
    k = 8  # drawn k times larger, then scaled down
    im = Image.new('RGBA', (GRID * k, H * k), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    g, y0 = GRID * k, TOP * k
    for row in range(3):
        for col in range(3):
            if warn and (col, row) == (2, 0):
                continue  # the red dot's place
            cx, cy, r = (12.5 + 37.5 * col) / 100 * g, y0 + (12.5 + 37.5 * row) / 100 * g, 12.5 / 100 * g
            a = 255 if (col, row) in lit else 102
            d.ellipse((cx - r, cy - r, cx + r, cy + r), fill=(255, 255, 255, a))
    if warn:  # a red dot a third of the grid wide in its top-right corner
        r = g / 6
        cx, cy = g - r, y0 + r
        d.ellipse((cx - r, cy - r, cx + r, cy + r), fill=(255, 59, 48, 255))
    return im.resize((GRID, H), Image.LANCZOS)


icon(ON).save(f'{out}/bar-on.png')
icon(set()).save(f'{out}/bar-off.png')
icon(ON, warn=True).save(f'{out}/bar-warn.png')
def disc(px, fill, dim):
    """The dots on a disc px wide: lit white, the others white at dim."""
    big = px * 4
    tile = Image.new('RGBA', (big, big), (0, 0, 0, 0))
    d = ImageDraw.Draw(tile)
    d.ellipse((0, 0, big - 1, big - 1), fill=fill)
    inner, off = big * 3 / 5, big / 5
    for row in range(3):
        for col in range(3):
            cx, cy, r = off + (12.5 + 37.5 * col) / 100 * inner, off + (12.5 + 37.5 * row) / 100 * inner, 12.5 / 100 * inner
            d.ellipse((cx - r, cy - r, cx + r, cy + r), fill=(255, 255, 255, 255 if (col, row) in ON else dim))
    return tile.resize((px, px), Image.LANCZOS)


for px, size in ((52, ''), (40, '-40')):
    # the app's own dark tile cut round, as the settings rows' icons are discs (stock's netservice_*.png)
    disc(px, (36, 36, 36, 255), 102).save(f'{out}/icon{size}.png')
    # Minimal (q2-pod's icon-<theme>.png): its settings discs' grey (#828282, measured in both halves)
    disc(px, (130, 130, 130, 255), 140).save(f'{out}/icon{size}-minimal.png')
