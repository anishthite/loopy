# User-authored JSON themes (standalone playground first)

Branch: `themes` (worktree `../loopy-themes`, off `main` @ b01ea52)

## What this does

Two things, in order:

1. **A standalone playground** (`internal/theme` + `cmd/themes`) you can run and
   poke at *right now*, decoupled from loopy's 116KB `internal/tui`. Author a
   theme JSON file, run `go run ./cmd/themes --dir <dir>`, pick it in the
   bubbletea picker, see a fake loopy transcript recolor live. Edit the file on
   disk → the transcript hot-reloads.
2. **(deferred)** Wiring `internal/theme` into the real TUI's six UI styles and
   the `/theme` panel. The package is shaped for this (built-ins carry today's
   exact indices, `Apply` is the single style-setting path) but the TUI wiring
   is a separate, smaller change once the playground feels right.

## Goal (standalone)

- The user can author `~/.loopy/themes/<name>.json` (or any `--dir`), run the
  playground, and see it applied to a representative transcript instantly.
- Editing the active theme's file on disk re-renders the transcript without a
  restart or re-pick (hot reload via dir-mtime poll — no fsnotify dep).
- Built-in `dark`/`light`/`auto` ship as themes carrying loopy's exact xterm-256
  indices, so they're first-class peers and the correctness anchor.

## Non-goals

- No in-app color-picker / live-editing widget. Authoring is "edit the JSON
  file, then `/theme` to re-pick." (ponytail: the file is the source of truth;
  a picker is a separate, much bigger surface.)
- No glamour/chroma *syntax-highlighting* palettes per theme. Those are
  glamour-internal; exposing them balloons the surface and the contrast work.
  Markdown keeps flipping between the existing glamour light/dark/ascii styles,
  driven by the theme's `dark`/`light`/`auto` background mode (see Design).
- No `{dark, light}` variant pairs inside one file (the fork from step 0). A
  theme declares one *background mode* — `dark`, `light`, or `auto` — and its
  colors. Simpler files, matches how `/theme` already reasons (one active
  scheme), and `auto` keeps doing exactly what it does today.
- No theme import/export, no theme gallery, no per-project themes.

## Design

### Surface

`internal/tui` only — no new tool, no agent-loop change, no persistence beyond
the existing `Config.Theme` string (which today holds `"light"`/`"dark"`/`""`).
The config field is *widened* to also accept a user theme name. `/theme` is the
only entry point, exactly as today.

### File format

`~/.loopy/themes/<name>.json`, JSONC (comments allowed — same parser as
config). `<name>` is `[a-z0-9_-]+`, lowercase, and must not collide with the
reserved built-in names `auto`/`light`/`dark`.

```jsonc
{
  // "dark" | "light" | "auto". Required. Drives the glamour markdown style
  // (dark/light/neutral-ascii) and lipgloss's has-dark-background flag, so
  // markdown + AdaptiveColor pick the right variant. "auto" means "render
  // markdown neutral, and let my colors stand on whatever bg the terminal
  // reports" — same as today's auto.
  "background": "dark",

  // Colors are xterm-256 indices (as strings, matching today's "12"/"245"
  // literals) OR hex "#1e1e2e". One value per role — no per-bg variant; the
  // `background` field already chose the mode.
  "colors": {
    "you":      "12",   // "You" speaker label  (today: dark 12)
    "bot":      "13",   // assistant label      (today: dark 13)
    "tool":     "11",   // tool-call headers    (today: dark 11)
    "dim":      "245",  // muted notes/borders  (today: dark 245)
    "error":    "9",    // error text           (today: dark 9)
    "thinking": "245"   // reasoning tokens     (today: dark 245, italic)
  }
  // every role is optional: a missing role falls back to the built-in default
  // for this theme's background mode, so a minimal theme can override one color
}
```

The six roles map 1:1 to today's six package-var styles in `tui.go`
(`youStyle`, `botStyle`, `toolStyle`, `dimStyle`, `errStyle`, `thinkingStyle`).
That is the entire color surface of the TUI today — nothing else is styled by
these theme vars, so the theme file covers the whole thing.

### New package: `internal/tui/theme`

Pure loader + applier, no I/O of its own except reading the themes dir (the
syscall edge). Keeps the theme struct, parsing, validation, and the live apply
out of the 116KB `tui.go`.

```go
// Role is a named color slot in the TUI.
type Role string
const (
    RoleYou      Role = "you"
    RoleBot      Role = "bot"
    RoleTool     Role = "tool"
    RoleDim      Role = "dim"
    RoleError    Role = "error"
    RoleThinking Role = "thinking"
)

// Theme is one user-authored or built-in color theme.
type Theme struct {
    Name       string          `json:"-"`
    Background string          `json:"background"`       // "dark"|"light"|"auto"
    Colors     map[Role]string `json:"colors,omitempty"`
}

// reserved built-in names — never a file theme
var reserved = map[string]bool{"auto": true, "light": true, "dark": true}

// Load reads ~/.loopy/themes/*.json. Returns built-ins + user themes, sorted.
// A malformed file is dropped with a logged warning (one bad theme never
// blocks the rest), never a hard error — themes are presentation, not infra.
func Load() ([]Theme, error)

// Apply makes `name` the active theme: sets the six lipgloss styles from it,
// sets lipgloss's has-dark-background, and calls SetLightTheme/SetUnknownTheme
// so markdown re-renders. Unknown name / "auto" falls back to detectColorScheme.
func Apply(name string) (note string, err error)

// Active reports the current theme name ("dark"/"light"/"auto"/user name) —
// the single source CurrentTheme() and the palette dynDesc read from.
func Active() string
```

`Apply` builds the six `lipgloss.NewStyle().Foreground(<color>).<Bold/Italic>`
from the theme's colors (resolving each role, falling back to today's literal
for the role when the theme omits it), then assigns them to the package-level
style vars. `thinkingStyle` keeps its `Italic(true)`; `youStyle`/`botStyle`
keep `Bold(true)` — those are role attributes, not colors, so they stay
hardcoded (a theme only swaps colors). Color resolution: a string that's all
digits → `lipgloss.Color(n)` (xterm-256, today's shape); a `#hex` →
`lipgloss.Color("#hex")`. Invalid color string → fall back to the role default
+ log, never panic.

### Built-ins are themes too

`internal/tui/theme` ships the three built-ins as `Theme` values with the exact
colors `tui.go` hardcodes today, so the live-apply path is one code path:

| role    | dark | light |
|---------|------|-------|
| you     | 12   | 21    |
| bot     | 13   | 90    |
| tool    | 11   | 136   |
| dim     | 245  | 240   |
| error   | 9    | 124   |
| thinking| 245  | 240   |

`auto` has no `Colors` — it runs `detectColorScheme` and then applies whichever
of dark/light it resolved (or neutral when unknown), i.e. today's behavior.
**This is the key correctness anchor**: because the built-ins carry today's
exact indices and `Apply` is the single path, switching to `dark`/`light`/`auto`
emits byte-identical SGR to today — every existing theme/contrast test stays
green with no special-casing.

### Markdown stays Glamour's job

`mdStyle()` already picks dark/light/ascii from `mdLight`/`mdKnown`. `Apply`
calls the existing `SetLightTheme`/`SetUnknownTheme` before re-render, so the
glamour style follows the theme's `background` field for free. User themes get
the dark/light/neutral glamour style matching their declared background —
consistent markdown without re-inventing the syntax palette (non-goal).

### `/theme` panel: list the user themes

`palette.go` builds the theme panel's `list` today as `["auto","light","dark"]`.
Change it to `theme.Load()` names: `["auto","light","dark", ...userThemeNames]`.
The panel kind stays `panelTheme`; the existing up/down/enter/left/right handler
calls `m.setTheme(pp.list[pp.midx])` unchanged — `setTheme` is widened to accept
a user theme name (below). Selection tracking (`pp.midx`) already works for any
list length. `dynDesc` ("current: X") reads `theme.Active()` instead of
`CurrentTheme()`.

### `setTheme` widening

`tui.go` `setTheme` today switches on `light`/`dark`/`default(auto)`. Widen it:
if the name is a user theme (in `theme.Load()`), call `theme.Apply(name)`,
persist `m.cfg.Theme = name`, `refreshVP()`, append the same `◐ theme: <name>`
note. `light`/`dark` keep their exact current bodies (they now also just route
through `theme.Apply`, but the SGR output is identical — see Built-ins above).
`auto` keeps its current `detectColorScheme` body verbatim. Config `Save` is
unchanged (atomic tmp+rename, already there); `Theme` is already a string field.

### Startup

`Run` calls `detectColorScheme()` at startup today. Add: if `cfg.Theme` names a
user theme, `theme.Apply(cfg.Theme)` instead. `cfgThemeValue` plumbing stays —
it still feeds the `light`/`dark` config override into detection; a user-theme
config value short-circuits to `Apply`. One-line branch in the existing startup
site, no new goroutine.

### Config schema

`Config.Theme` doc comment widens from `"light"/"dark"/""` to
`"light"/"dark"/""(auto)/<user theme name>`. No struct change, no migration —
the field is already an opaque string. `Default()` still returns `""` (auto).

## Files touched

- **new** `internal/tui/theme.go` — `Theme`, `Load`, `Apply`, `Active`, built-ins.
- **new** `internal/tui/theme_test.go` (appended to — file exists) — loader,
  apply, built-in-exactness, fallback tests.
- **edit** `internal/tui/tui.go` — widen `setTheme`; startup `Apply` branch;
  convert the six style vars from `var (...)` to package vars assigned by
  `theme.Apply` (initialized to the dark built-in at package load so tests
  that don't call Apply still see today's colors).
- **edit** `internal/tui/palette.go` — theme panel `list` from `theme.Load()`;
  `dynDesc` from `theme.Active()`.
- **edit** `internal/config/config.go` — widen `Theme` doc comment only.
- **edit** `docs/features.md` — new "Themes" subsection (behavior → code → tests).
- **edit** `docs/roadmap.md` — check the "Theme support" box (partial: JSON
  themes + live switch landed; `{dark,light}` pairs + system-from-terminal
  palette deferred — note in the checkbox).
- **edit** `README.md` — short "Themes" note if user-facing (the `/theme` +
  `~/.loopy/themes/` path is user-facing). Confirm during build.

## Test plan

Use stdlib `testing`, race clean (`go test -race ./internal/tui/...`).

1. **Built-in exactness** (the correctness anchor) — `theme_test.go`: after
   `theme.Apply("dark")` then `theme.Apply("light")`, `renderMarkdown` emits the
   same `38;5;234`/`252`/`251`/`124`/`255` sequences the existing
   `theme_test.go`/`theme_contrast_test.go` assert. Run the *existing* tests
   unchanged — they must pass as-is (no edits to them). This proves the built-ins
   didn't drift.
2. **Loader** — write a temp themes dir (t.Setenv `HOME`), parse a valid theme,
   a theme missing some roles (fallback), a `#hex` color, a malformed file
   (dropped, not fatal), a reserved-name file (rejected). `Load` sorts, built-ins
   first.
3. **Apply** — a user theme sets the six styles to its colors; omitted role
   falls back to the built-in for that `background`; invalid color falls back +
   doesn't panic; `Active()` reflects the name; markdown re-renders under the
   theme's background mode.
4. **`/theme` panel** — bare `/theme` lists built-ins + a temp-dir user theme;
   selecting the user theme applies it live (`Active()` == name) and pops the
   panel; selecting `dark`/`light`/`auto` behaves exactly as the existing
   `theme_cmd_test.go` asserts (run it unchanged).
5. **Persistence** — selecting a user theme sets `cfg.Theme` and survives
   `cfg.Save()`/`Load()` round-trip; startup `Apply`s it.
6. **Resume** — N/A: themes are live presentation, not message-adjacent state.
   A resumed session re-applies `cfg.Theme` at startup and re-renders; no new
   persisted shape. Note as intentionally live-only.

## Docs plan

- `docs/features.md`: a "Themes" entry under the Display cluster — behavior
  (`~/.loopy/themes/<name>.json`, `/theme` lists+switches live, built-ins are
  first-class), code (`internal/tui/theme.go`, `setTheme`, palette panel), tests
  (the 6 above, named).
- `docs/roadmap.md`: check the "Theme support" line, annotate what's deferred
  (`{dark,light}` pairs, system-from-terminal-palette theme).
- `README.md`: one line under config/UI — "Themes: drop a JSON file in
  `~/.loopy/themes/` and pick it with `/theme`." if the README has a config
  section.

## Ordered task breakdown

1. `internal/tui/theme.go`: `Theme`/`Role`/built-ins/`Load`/`Apply`/`Active`,
   color resolution + fallback, no I/O coupling. Unit-testable in isolation.
2. `internal/tui/theme_test.go`: loader + apply + built-in-exactness tests.
   Run existing `theme_test.go`/`theme_contrast_test.go`/`theme_cmd_test.go`
   untouched — green proves no drift.
3. `tui.go`: convert the six style vars to `Apply`-assigned package vars
   (init to dark built-in); widen `setTheme` to route user names through
   `theme.Apply`; startup branch.
4. `palette.go`: theme panel `list` from `theme.Load()`; `dynDesc` from
   `theme.Active()`.
5. `config.go`: widen `Theme` doc comment.
6. `task check` (+ `-race` on `internal/tui`). Adversarial pass: parallel
   `/theme` calls? (no — palette is single-keyed, setTheme is synchronous in the
   Update loop), ctrl+c mid-apply? (Apply is sync, no goroutine), resume?
   (live-only, noted), bad theme file? (dropped, logged), clobbered config?
   (existing Save guard covers it).
7. Docs: `features.md`, `roadmap.md`, `README.md` (if it has a UI/config bit).

## Prior art

- opencode `theme/index.ts` — 34 JSON themes, `defs` + `{dark,light}` pairs,
  layered defaults<plugins<user<system. Research in
  `docs/learnings/other-harnesses/opencode/opencode-ux.md` §5. This plan is the
  ponytail slice: one file per theme, one background mode, the six UI roles, live
  `/theme` switch — the `{dark,light}` pair + system-from-terminal-palette ideas
  are explicitly deferred (non-goals) and noted in the roadmap checkbox.
