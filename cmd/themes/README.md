# themes — loopy theme playground

A standalone playground for authoring and testing loopy TUI color themes,
without running the full agent. Edit a JSON file, pick it, see it live.

This lives in the `themes` worktree (`../loopy-themes`, branch `themes`) and is
self-contained: `internal/theme` has no dependency on loopy's `internal/tui`,
and `cmd/themes` drives it directly.

## Run it

```sh
# from the worktree root (../loopy-themes)
go run ./cmd/themes --dir ./cmd/themes/examples   # use the shipped example themes
go run ./cmd/themes                                # use ~/.loopy/themes
```

You'll get a fake loopy transcript (a user turn, reasoning, an assistant
markdown reply with a code block + table, a tool call, and an error note) and a
theme picker along the bottom.

## Keys

| key | action |
|-----|--------|
| `↑`/`↓` or `j`/`k` | move the theme cursor |
| `enter` / `←` / `→` | apply the highlighted theme (re-renders the transcript live) |
| `t` | open / close the theme picker |
| `r` | force a hot-reload of the themes dir |
| `↑`/`↓`/`j`/`k` | scroll the transcript when the picker is closed |
| `pgup` / `pgdown` | scroll the transcript by half-pages |
| `q` / `ctrl+c` | quit |

## Author a theme

Drop a JSON file in the themes dir (`~/.loopy/themes/<name>.json` or wherever
you pointed `--dir`). The file is JSONC — `//` line comments are allowed.

```jsonc
{
  // "dark" | "light" | "auto". Required. Selects the markdown style and
  // lipgloss's has-dark-background flag, so markdown renders with matching
  // contrast. "auto" defers to the terminal's reported background.
  "background": "dark",

  // Colors are xterm-256 indices (e.g. "12") OR "#rrggbb" / "#rgb" hex.
  // Every role is optional — a missing role falls back to the built-in
  // default for this theme's background, so a minimal theme can override
  // just one color.
  "colors": {
    "you":      "#88c0d0", // the "You" speaker label
    "bot":      "#b48ead", // the "Assistant" speaker label
    "tool":     "#ebcb8b", // tool-call headers
    "dim":      "#4c566a", // muted notes, borders, separators
    "error":    "#bf616a", // error text
    "thinking": "#4c566a"  // reasoning tokens (always italic)
  }
}
```

**That's the whole color surface** — those six roles are everything the TUI
colors. Role attributes are fixed by role (you/bot are bold, thinking is
italic), so a theme only swaps colors; it can't accidentally drop the bold
labels or the italic reasoning style.

### Rules

- `<name>` is the filename without `.json`: lowercase `[a-z0-9_-]+`.
- `auto`, `light`, `dark` are reserved built-in names — a file named
  `dark.json` is rejected (it can't shadow the built-in).
- A malformed file (bad JSON, unknown background, unknown role, bad color) is
  **dropped with a warning on stderr** and the rest still load — one bad theme
  never blocks the others.

### Hot reload

Save the file (or drop in a new one) and the picker refreshes automatically —
the dir's mtimes are polled, no file-watcher dependency. If you edited the
*active* theme, the transcript re-renders immediately so you see the change
without re-picking. `r` forces a reload if the poll hasn't caught it yet.

## Built-in themes

`auto`, `light`, `dark` always appear first in the picker. They carry the exact
xterm-256 indices loopy's TUI hardcodes today (`you` 12/21, `bot` 13/90, `tool`
11/136, `dim` 245/240, `error` 9/124, `thinking` 245/240 — dark/light), so
switching to a built-in is byte-identical to the pre-theme TUI. `auto` resolves
to dark/light via the terminal's reported background (or a neutral default when
undetermined).

## Shipped examples

`cmd/themes/examples/` — well-known palettes to start from or copy:

- `nord.json` — Nord (arctic, dark)
- `gruvbox.json` — Gruvbox (warm, dark)
- `catppuccin.json` — Catppuccin Mocha (dark)
- `solarized-light.json` — Solarized Light (light bg)

```sh
go run ./cmd/themes --dir ./cmd/themes/examples
```

## Package: `internal/theme`

The reusable core. The loopy TUI will wire its existing six UI styles and
`/theme` panel onto this package; the playground drives it directly.

- `theme.LoadFrom(dir)` — parse a themes dir → `[]Theme` (built-ins first).
- `theme.Apply(name, detect)` — make `name` active; rebuilds the `StyleSet`,
  sets lipgloss's background flag, and re-renders markdown. `detect` resolves
  `auto`.
- `theme.Styles()` — snapshot of the six `lipgloss.Style`s + resolved colors.
- `theme.RenderMarkdown(s, width)` — glamour markdown under the active theme's
  background.

Tests: `go test -race ./internal/theme/` — built-in-exactness (the correctness
anchor: applying dark/light emits the same SGR the TUI does today), loader
(valid/missing-role/hex/malformed/reserved-name/JSONC/missing-dir/reload),
apply + auto/detector, color resolution, role-attribute fixity.
