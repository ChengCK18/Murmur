#!/usr/bin/env python3
"""Draws docs/img/*.svg from loadtest/results/. Standard library only.

The charts follow the reader's light or dark theme. They have no hover layer:
GitHub shows an SVG as an image, so docs/results.md has the numbers in tables.
"""
import json
import math
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
W, H = 720, 360
LEFT, RIGHT, TOP, BOTTOM = 72, 96, 84, 56  # plot margins; the right holds line labels
STYLE = """<style>
  svg { --surface:#fcfcfb; --ink:#0b0b0b; --ink2:#52514e; --muted:#898781; --grid:#e1e0d9; --axis:#c3c2b7; --after:#2a78d6; --before:#898781; --ref:#c3c2b7; }
  @media (prefers-color-scheme: dark) {
    svg { --surface:#1a1a19; --ink:#f0efec; --ink2:#c3c2b7; --muted:#898781; --grid:#2c2c2a; --axis:#383835; --after:#3987e5; --before:#898781; --ref:#6b6a64; }
  }
  text { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; fill: var(--ink2); font-size: 13px; font-variant-numeric: tabular-nums; }
  .title { fill: var(--ink); font-size: 16px; font-weight: 600; }
  .sub { font-size: 13px; }
  .tick { fill: var(--muted); font-size: 12px; }
  .label { fill: var(--ink); font-size: 13px; }
</style>"""


def load(name):
    return json.loads((ROOT / "loadtest/results" / f"{name}.json").read_text())


def fleet(n):
    return f"{n:,}"


class Chart:
    def __init__(self, title, sub, xmax, xticks, y, yticks, yfmt, xlabel="Drones (10 messages a second each)"):
        self.x = lambda v: LEFT + v / xmax * (W - LEFT - RIGHT)
        self.y = lambda v: H - BOTTOM - y(v) * (H - TOP - BOTTOM)
        self.parts = [
            f'<rect width="{W}" height="{H}" fill="var(--surface)"/>',
            f'<text class="title" x="20" y="28">{title}</text>',
            f'<text class="sub" x="20" y="48">{sub}</text>',
        ]
        for t in yticks:
            py = self.y(t)
            self.parts.append(f'<line x1="{LEFT}" x2="{W - RIGHT}" y1="{py:.1f}" y2="{py:.1f}" stroke="var(--grid)"/>')
            self.parts.append(f'<text class="tick" x="{LEFT - 8}" y="{py + 4:.1f}" text-anchor="end">{yfmt(t)}</text>')
        base = H - BOTTOM
        self.parts.append(f'<line x1="{LEFT}" x2="{W - RIGHT}" y1="{base}" y2="{base}" stroke="var(--axis)"/>')
        for t in xticks:
            self.parts.append(f'<text class="tick" x="{self.x(t):.1f}" y="{base + 18}" text-anchor="middle">{fleet(t)}</text>')
        self.parts.append(f'<text class="tick" x="{(LEFT + W - RIGHT) / 2}" y="{H - 10}" text-anchor="middle">{xlabel}</text>')
        self.legend = []

    def line(self, name, points, color, label=None, dy=4, width=2):
        pts = " ".join(f"{self.x(x):.1f},{self.y(v):.1f}" for x, v in points)
        self.parts.append(f'<polyline points="{pts}" fill="none" stroke="var({color})" stroke-width="{width}" stroke-linecap="round" stroke-linejoin="round"/>')
        for x, v in points:
            self.parts.append(f'<circle cx="{self.x(x):.1f}" cy="{self.y(v):.1f}" r="4" fill="var({color})" stroke="var(--surface)" stroke-width="2"/>')
        x, v = points[-1]
        self.parts.append(f'<text class="label" x="{self.x(x) + 9:.1f}" y="{self.y(v) + dy:.1f}">{label or name}</text>')
        self.legend.append((name, color))

    def reference(self, name, points):
        pts = " ".join(f"{self.x(x):.1f},{self.y(v):.1f}" for x, v in points)
        self.parts.append(f'<polyline points="{pts}" fill="none" stroke="var(--ref)" stroke-width="2" stroke-linecap="round"/>')
        self.legend.append((name, "--ref"))

    def note(self, x, v, text, dx=0, dy=-10, anchor="middle"):
        self.parts.append(f'<text class="tick" x="{self.x(x) + dx:.1f}" y="{self.y(v) + dy:.1f}" text-anchor="{anchor}">{text}</text>')

    def save(self, name):
        x = 20
        for label, color in self.legend:
            self.parts.append(f'<rect x="{x}" y="60" width="12" height="4" rx="2" fill="var({color})"/>')
            self.parts.append(f'<text x="{x + 17}" y="66" class="tick">{label}</text>')
            x += 28 + 7 * len(label)
        svg = f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" role="img" aria-label="{name}">\n{STYLE}\n' + "\n".join(self.parts) + "\n</svg>\n"
        (ROOT / "docs/img" / f"{name}.svg").write_text(svg)


def main():
    before, after = load("before"), load("after") + load("ceiling")

    c = Chart("Rows reach the database as fast as drones send them, up to about 14,000 a second",
              "Rows written per second, measured over 60 s at each fleet size",
              3000, [100, 500, 1000, 1500, 2000, 3000], lambda v: v / 32000,
              [0, 8000, 16000, 24000, 32000], lambda v: f"{v:,}")
    c.reference("Sent by the drones", [(0, 0), (3000, 30000)])
    c.line("Before", [(r["drones"], r["inserted_per_s"]) for r in before], "--before", dy=16)
    c.line("After", [(r["drones"], r["inserted_per_s"]) for r in after], "--after")
    c.note(1000, 10000, "the 10,000 a second target", dx=-8, dy=-14, anchor="end")
    c.save("throughput")

    c = Chart("Packets no longer vanish on the way to the ingester",
              "Share of packets sent that the ingester never read",
              1000, [100, 250, 500, 1000], lambda v: v / 12, [0, 4, 8, 12], lambda v: f"{v}%")
    c.line("Before", [(r["drones"], r["loss_pct"]) for r in before], "--before")
    c.line("After", [(r["drones"], r["loss_pct"]) for r in load("after")], "--after")
    c.save("loss")

    log = lambda v: math.log2(v / 100) / math.log2(320)
    c = Chart("Half a second from packet to database, up to the 10,000 a second target",
              "Receive to database commit, p99, on a log scale",
              3000, [100, 500, 1000, 1500, 2000, 3000], log,
              [100, 1000, 10000, 32000], lambda v: f"{v / 1000:g} s")
    c.line("Before", [(r["drones"], r["commit_p99_ms"]) for r in before], "--before", dy=-8)
    c.line("After", [(r["drones"], r["commit_p99_ms"]) for r in after], "--after")
    c.note(2500, 16384, "16 s is the top of the histogram: the true value is higher", dy=-12)
    c.save("latency")


if __name__ == "__main__":
    main()
