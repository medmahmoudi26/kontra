# FiraCode Nerd Font — provenance and licence

`FiraCodeNerdFont-Regular.woff2` is vendored **verbatim**, and this file records how its licence was
established, because ADR 0020's slice brief forbids vendoring a font without one.

## Why a Nerd Font at all

The Dashboard renders what a Worker prints. `journalctl` output, `systemd` status glyphs, progress
bars and anything drawn with box characters land in a tile as U+2500–U+257F (box drawing),
U+2580–U+259F (block elements) and — from anything with a powerline prompt or a Nerd-Font-aware TUI —
the private-use ranges U+E0A0–U+E0D4 and U+F000+. A generic `monospace` stack renders those as
tofu, and a wall of tofu reads as broken output rather than as a missing glyph. This is the single
largest perceived-quality difference on the wall.

## What was shipped

| | |
|---|---|
| File | `FiraCodeNerdFont-Regular.woff2` |
| SHA-256 | `ef53992cdf469d4024d2efc29d51c05a65425d1ca690e505fab80223980a067d` |
| Bytes | 1 108 572 (unmodified; not subset) |
| Family (`name` ID 1) | FiraCode Nerd Font |
| Version (`name` ID 5) | Version 6.002;Nerd Fonts 3.3.0 |
| Copyright (`name` ID 0) | Copyright 2014-2021 The Fira Code Project Authors (https://github.com/tonsky/FiraCode) |
| Designers (`name` ID 9) | Carrois Corporate, Edenspiekermann AG, Nikita Prokopov |
| Trademark (`name` ID 7) | Fira Mono is a trademark of The Mozilla Corporation. |
| Licence (`name` ID 13) | This Font Software is licensed under the SIL Open Font License, Version 1.1. |
| Licence URL (`name` ID 14) | http://scripts.sil.org/OFL |
| `OS/2.fsType` | `0` — Installable Embedding, no restriction |
| Licence text | `LICENSE-OFL-1.1.txt`, beside the font |

Copied from `flplima/tmuxy` (MIT), which is the reference implementation ADR 0020's slice 7 amendment
names — but note that **tmuxy ships the woff2 with no licence text of its own**, so its repo-level MIT
`LICENSE` (which covers tmuxy's code, not a bundled third-party font) was not evidence of anything.
The licence was established from the font binary instead.

## How the licence was established, and how to re-check it

The `name` table rows above are not a guess — they were read out of the file itself, which is where a
font records its own licence:

```
python3 -c "
from fontTools.ttLib import TTFont
f = TTFont('FiraCodeNerdFont-Regular.woff2')
for r in f['name'].names:
    if r.nameID in (0, 5, 13, 14):
        print(r.nameID, r.toUnicode())
print('fsType', f['OS/2'].fsType)"
```

`name` ID 13 states OFL-1.1 and ID 14 gives SIL's URL, so the licence is the font's own declaration
rather than an inference from the upstream project's README.

`LICENSE-OFL-1.1.txt` is the copyright notice from `name` ID 0 followed by the canonical OFL-1.1 body.
The body was **not** typed from memory: it was taken from a copy already on the build host and
verified byte-identical against a second, independent copy of OFL-1.1 from a different font project
(`diff` of the two, from the `-----` ruler to the end: no differences). OFL-1.1 requires the licence
to travel unmodified, so that check is the point of it.

## What the licence requires of us, and where each obligation is met

OFL-1.1 §2: *"Original or Modified Versions of the Font Software may be bundled, redistributed and/or
sold with any software, provided that each copy contains the above copyright notice and this
license."*

- The copyright notice and the licence ship in `LICENSE-OFL-1.1.txt`, in this directory, beside the
  font — not in a distant `THIRD-PARTY` file that a `vite build` would drop.
- The font is **unmodified**. It is deliberately not subset: a subset is a Modified Version, which
  adds an obligation (and a build step) for a saving that does not matter here. See "Cost" below.
- FiraCode carries **no Reserved Font Name** (its copyright line has no `with Reserved Font Name`
  clause, which is why "FiraCode Nerd Font" is a legitimate name for the Nerd Fonts patch in the
  first place), so §3 imposes no rename on us.
- OFL-1.1 §1 forbids selling the Font Software *by itself*. Nothing here does.

## Cost, and the fallback

1.08 MB, fetched once per browser and then cached, and only by a tab that opens the Dashboard —
`App.tsx` lazy-loads `DashboardPage`, and `fonts/nerd-font.css` is imported from `panels/theme.ts`
inside that chunk, so the workflow editor never pays for it.

`theme.ts`'s `TERMINAL_FONT_STACK` lists the Nerd Font FIRST and then a documented fallback chain, so
a build that deletes this directory degrades to system monospace instead of failing to compile. What
it loses is exactly the glyph coverage described above.

The font arrives AFTER the first paint (`font-display: swap`), which changes xterm's cell metrics
under a terminal that has already measured itself. That is not cosmetic on this page — the measured
cols/rows are what the streamer puts into `stty` before `tmux attach` — so `theme.ts` exports
`whenTerminalFontReady()` and `TerminalTile` re-measures once it resolves. See the comments there.
