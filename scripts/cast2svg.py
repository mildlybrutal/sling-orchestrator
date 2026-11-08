#!/usr/bin/env python3
"""Turns an asciicast v2 recording into a looping animated SVG for the README.

    python3 scripts/cast2svg.py in.cast out.svg [--rows 30] [--max-gap 1.6]

Each output line is drawn once, fades in at the moment it was printed, and the
window scrolls as a terminal would. Pauses longer than --max-gap seconds are
shortened (a 30s grace window plays in under two); the durations Sling prints
are the real ones. Understands SGR colours; drops other escape sequences, so
record with progress bars off (BUILDKIT_PROGRESS=quiet).
"""
import argparse, html, json, re

FG = {"30": "#5b6478", "31": "#ff6b81", "32": "#4ade80", "33": "#fbbf24", "34": "#7aa2ff",
      "35": "#c792ea", "36": "#5fd4ff", "37": "#e8eefc", "90": "#7d88a3", "91": "#ff8fa0",
      "92": "#86efac", "93": "#fde68a", "94": "#9fc0ff", "95": "#e0b0ff", "96": "#a5f0ff", "97": "#ffffff"}
INK, DIM, BG = "#d7def0", "#7d88a3", "#0a1122"
# Sling prints plain text; these only colour whole lines so the outcomes stand out.
HIGHLIGHT = [(re.compile(r"^(✅|Hello from)"), "#4ade80"),
             (re.compile(r"^(deploy failed|\s+(failed|rolled_back)\s)"), "#ff6b81"),
             # `sling history` rows: date, time, kind and commit come before the status
             (re.compile(r"^(\S+\s+){5}(rolled_back|failed)\s"), "#ff8fa0"),
             (re.compile(r"^(\S+\s+){5}live\b"), "#86efac")]
ESC = re.compile(r"\x1b\[([0-9;?]*)([A-Za-z])|\x1b[()][A-Za-z0-9]|\x1b[=>]")


def parse(cast, max_gap):
    """[(time, [(text, style)])] one entry per finished line, on a compressed clock."""
    events = [json.loads(l) for l in open(cast, encoding="utf-8").read().splitlines()[1:]]
    lines, cur, style = [], [], {}
    clock, last = 0.0, 0.0
    for t, kind, data in events:
        if kind != "o":
            continue
        clock += min(t - last, max_gap)
        last = t
        pos = 0
        for m in list(ESC.finditer(data)) + [None]:
            chunk = data[pos:m.start() if m else len(data)]
            parts = chunk.replace("\r\n", "\n").split("\n")
            for i, part in enumerate(parts):
                part = part.split("\r")[-1] if "\r" in part else part
                if part:
                    cur.append((part, dict(style)))
                if i < len(parts) - 1:
                    lines.append((clock, cur))
                    cur = []
            if m is None:
                break
            pos = m.end()
            if m.group(2) == "m":
                for code in (m.group(1) or "0").split(";"):
                    if code in ("", "0"):
                        style = {}
                    elif code == "1":
                        style["b"] = True
                    elif code == "2":
                        style["dim"] = True
                    elif code in FG:
                        style["fg"] = FG[code]
                    elif code == "39":
                        style.pop("fg", None)
                    elif code == "22":
                        style.pop("b", None), style.pop("dim", None)
    if cur:
        lines.append((clock, cur))
    return lines, clock


def wrap(lines, cols):
    """Wrap each line at cols, as the terminal did, and apply the line highlights."""
    out = []
    for t, spans in lines:
        text = "".join(x for x, _ in spans)
        colour = next((c for rx, c in HIGHLIGHT if rx.search(text)), None)
        if colour:
            spans = [(x, {**st, "fg": st.get("fg", colour)}) for x, st in spans]
        row, n = [], 0
        for x, st in spans:
            while x:
                take = x[:cols - n]
                row.append((take, st)); n += len(take); x = x[len(take):]
                if n == cols:
                    out.append((t, row)); row, n = [], 0
        if row or not spans:
            out.append((t, row))
    return out


def render(lines, total, rows, cols, hold=4.0):
    cw, lh, pad, bar = 8.2, 19, 18, 34
    w = int(cols * cw + 2 * pad)
    h = int(rows * lh + 2 * pad + bar)
    dur = total + hold
    pct = lambda t: f"{min(t / dur * 100, 99.99):.3f}"
    css, body, scroll = [], [], []
    for i, (t, spans) in enumerate(lines):
        p = pct(t)
        css.append(f"@keyframes a{i}{{0%,{p}%{{opacity:0}}{float(p) + .01:.3f}%,100%{{opacity:1}}}}"
                   f".l{i}{{animation:a{i} {dur:.2f}s step-end infinite}}")
        tspans = []
        for text, st in spans:
            attrs = []
            if "fg" in st:
                attrs.append(f'fill="{st["fg"]}"')
            elif st.get("dim"):
                attrs.append(f'fill="{DIM}"')
            if st.get("b"):
                attrs.append('font-weight="700"')
            tspans.append(f"<tspan {' '.join(attrs)}>{html.escape(text)}</tspan>" if attrs else html.escape(text))
        y = pad + bar + (i + 1) * lh - 5
        body.append(f'<text class="l{i}" x="{pad}" y="{y}" xml:space="preserve">{"".join(tspans)}</text>')
        if i >= rows:
            scroll.append((t, (i + 1 - rows) * lh))
    frames = ["0%{transform:translateY(0)}"]
    for t, off in scroll:
        frames.append(f"{pct(t)}%{{transform:translateY(-{off}px)}}")
    frames.append(f"100%{{transform:translateY(-{scroll[-1][1] if scroll else 0}px)}}")
    css.append(f"@keyframes s{{{''.join(frames)}}}.s{{animation:s {dur:.2f}s step-end infinite}}")
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}" role="img" aria-label="Sling deploying three commits: one goes live, one crashes and never gets traffic, one is rolled back automatically">
<style>text{{font:13.5px/1 "JetBrains Mono","SF Mono",Menlo,Consolas,monospace;fill:{INK}}}{"".join(css)}@media (prefers-reduced-motion:reduce){{[class]{{animation:none!important}}}}</style>
<rect width="{w}" height="{h}" rx="12" fill="{BG}"/>
<rect x=".5" y=".5" width="{w - 1}" height="{h - 1}" rx="12" fill="none" stroke="#24304f"/>
<circle cx="22" cy="17" r="5.5" fill="#ff5f57"/><circle cx="40" cy="17" r="5.5" fill="#febc2e"/><circle cx="58" cy="17" r="5.5" fill="#28c840"/>
<text x="{w / 2}" y="21" text-anchor="middle" fill="{DIM}" style="font-size:12px">sling — demo</text>
<line x1="0" y1="{bar}" x2="{w}" y2="{bar}" stroke="#1b2540"/>
<clipPath id="c"><rect x="0" y="{bar + 4}" width="{w}" height="{h - bar - 8}"/></clipPath>
<g clip-path="url(#c)"><g class="s">
{chr(10).join(body)}
</g></g>
</svg>
'''


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cast"), ap.add_argument("svg")
    ap.add_argument("--rows", type=int, default=30)
    ap.add_argument("--cols", type=int, default=100)
    ap.add_argument("--max-gap", type=float, default=1.6)
    a = ap.parse_args()
    lines, total = parse(a.cast, a.max_gap)
    lines = wrap(lines, a.cols)
    open(a.svg, "w", encoding="utf-8").write(render(lines, total, a.rows, a.cols))
    print(f"{a.svg}: {len(lines)} lines, {total + 4:.1f}s loop")


if __name__ == "__main__":
    main()
