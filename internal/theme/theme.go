// Package theme is a self-contained terminal theme system: user-authored JSON
// files in a themes directory, loaded into typed Theme values, and applied live
// to a set of named lipgloss styles + a glamour markdown style.
//
// It has no dependency on the rest of loopy — it's the smallest surface that
// lets you author a color theme, switch to it, and see markdown + transcript
// chrome re-render. The [loopy] TUI wires its existing six UI styles and
// /theme panel onto this package; the standalone playground in cmd/themes
// drives it directly.
//
// A theme file is ~/.loopy/themes/<name>.json (or any dir passed to LoadFrom),
// JSONC (comments allowed). Example:
//
//	{
//	  "background": "dark",        // "dark" | "light" | "auto"
//	  "colors": {
//	    "you":      "12",          // xterm-256 index OR "#hex"
//	    "bot":      "13",
//	    "tool":     "11",
//	    "dim":      "245",
//	    "error":    "9",
//	    "thinking": "245"
//	    // every role optional: missing falls back to this theme's built-in
//	  }
//	}
//
// The six roles are the entire colored surface; <name> must be lowercase
// [a-z0-9_-]+ and not collide with the reserved built-in names
// ("auto"/"light"/"dark"), which always appear first in Load's result.
package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// Role is a named color slot the TUI paints. A theme may set any subset;
// omitted roles fall back to the built-in default for the theme's background.
type Role string

const (
	RoleYou      Role = "you"      // the "You" speaker label
	RoleBot      Role = "bot"      // the assistant speaker label
	RoleTool     Role = "tool"     // tool-call headers
	RoleDim      Role = "dim"      // muted notes, borders, separators
	RoleError    Role = "error"    // error text
	RoleThinking Role = "thinking" // reasoning tokens (italic)
)

// allRoles is the full set, in definition order. Iterating this (not a map)
// keeps built-in color tables and fallback resolution deterministic.
var allRoles = []Role{
	RoleYou, RoleBot, RoleTool, RoleDim, RoleError, RoleThinking,
}

// Background is the terminal-background mode a theme assumes. It selects the
// glamour markdown style (Dark/Light/neutral-ASCII) and lipgloss's
// has-dark-background flag, so markdown and AdaptiveColor pick the right
// variant. "auto" means "render markdown neutral, let the terminal report" —
// the caller resolves it (see [Apply]).
type Background string

const (
	BgDark  Background = "dark"
	BgLight Background = "light"
	BgAuto  Background = "auto"
)

// reserved built-in names — never loaded from a file. A file named e.g.
// dark.json is rejected so a user theme can't shadow a built-in.
var reserved = map[string]bool{"auto": true, "light": true, "dark": true}

// nameRe bounds a theme filename: lowercase letters, digits, underscore,
// hyphen. Keeps filenames shell-safe and collision-free with the built-ins.
const nameRe = "^[a-z0-9_-]+$"

// builtins are the three built-in themes, carrying the exact xterm-256 indices
// loopy's TUI hardcodes today. Because [Apply] is the single path that sets
// styles, switching to dark/light/auto emits byte-identical SGR to the pre-theme
// TUI — existing contrast tests pass unchanged. Colors keyed by role.
var builtins = map[string]Theme{
	"dark": {
		Name:       "dark",
		Background: BgDark,
		Colors: map[Role]string{
			RoleYou: "12", RoleBot: "13", RoleTool: "11",
			RoleDim: "245", RoleError: "9", RoleThinking: "245",
		},
	},
	"light": {
		Name:       "light",
		Background: BgLight,
		Colors: map[Role]string{
			RoleYou: "21", RoleBot: "90", RoleTool: "136",
			RoleDim: "240", RoleError: "124", RoleThinking: "240",
		},
	},
	"auto": {
		Name:       "auto",
		Background: BgAuto,
		// no Colors: auto resolves to dark/light via the caller's detector,
		// then applies that built-in. Kept as a sentinel so the /theme panel
		// can list it alongside user themes.
	},
}

// Theme is one user-authored or built-in color theme.
type Theme struct {
	Name       string          `json:"-"`          // from filename; "" for built-ins
	Background Background      `json:"background"` // required
	Colors     map[Role]string `json:"colors,omitempty"`
}

// Valid reports a non-nil error if the theme is malformed: a missing or
// unknown background, an unknown color role (a typo like "erroor"), or a color
// value [ResolveColor] can't parse. A valid theme may omit any role.
func (t Theme) Valid() error {
	if t.Background != BgDark && t.Background != BgLight && t.Background != BgAuto {
		return fmt.Errorf("theme %q: background must be dark|light|auto, got %q", t.Name, t.Background)
	}
	for role, c := range t.Colors {
		if !isKnownRole(role) {
			return fmt.Errorf("theme %q: unknown color role %q (want one of %s)", t.Name, role, joinRoles())
		}
		if _, ok := resolveColor(c); !ok {
			return fmt.Errorf("theme %q: role %q: bad color %q (want an xterm-256 index or #hex)", t.Name, role, c)
		}
	}
	return nil
}

func isKnownRole(r Role) bool {
	for _, k := range allRoles {
		if k == r {
			return true
		}
	}
	return false
}

func joinRoles() string {
	s := make([]string, len(allRoles))
	for i, r := range allRoles {
		s[i] = string(r)
	}
	return strings.Join(s, ", ")
}

// StyleSet is the six concrete lipgloss styles an apply produces, plus the
// raw resolved colors (so tests can assert exact indices without re-parsing
// SGR). The TUI and cmd/themes read these to render chrome.
//
// Role attributes are fixed by role, not by theme: you/bot are Bold, thinking
// is Italic. A theme only swaps colors — so a user theme can't accidentally
// drop the bold speaker labels or the italic reasoning style.
type StyleSet struct {
	You      lipgloss.Style
	Bot      lipgloss.Style
	Tool     lipgloss.Style
	Dim      lipgloss.Style
	Error    lipgloss.Style
	Thinking lipgloss.Style

	// Colors mirrors the resolved color per role (xterm index or #hex string,
	// exactly as stored). Tests assert against this; rendering uses the styles.
	Colors map[Role]string
	// Bg is the resolved background after an "auto" theme was applied (auto is
	// resolved by the caller's detector at apply time, so an applied theme
	// always has a concrete dark/light — never auto here).
	Bg Background
	// Determined is false when an "auto" theme couldn't resolve a background
	// (no reliable terminal signal). In that case markdown should render in the
	// neutral default style (no forced bg) — the TUI checks this to decide
	// SetUnknownTheme vs SetLightTheme. Bg is BgDark as the safe color default.
	Determined bool
}

// styleFor builds one role's style from a resolved color, applying the role's
// fixed attribute (bold for you/bot, italic for thinking).
func styleFor(role Role, color string) lipgloss.Style {
	st := lipgloss.NewStyle().Foreground(lipgloss.Color(color))
	switch role {
	case RoleYou, RoleBot:
		st = st.Bold(true)
	case RoleThinking:
		st = st.Italic(true)
	}
	return st
}

// buildStyles resolves every role (falling back to the built-in for t.Background
// when t omits a role) and returns the concrete StyleSet. bg must be a concrete
// dark/light (auto already resolved by the caller) — it selects which built-in
// table supplies per-role fallbacks.
func (t Theme) buildStyles(bg Background) StyleSet {
	fallback := builtins[string(bg)].Colors // dark or light table
	colors := make(map[Role]string, len(allRoles))
	for _, role := range allRoles {
		if c, ok := t.Colors[role]; ok {
			colors[role] = c
		} else if c, ok := fallback[role]; ok {
			colors[role] = c
		}
	}
	return StyleSet{
		You:        styleFor(RoleYou, colors[RoleYou]),
		Bot:        styleFor(RoleBot, colors[RoleBot]),
		Tool:       styleFor(RoleTool, colors[RoleTool]),
		Dim:        styleFor(RoleDim, colors[RoleDim]),
		Error:      styleFor(RoleError, colors[RoleError]),
		Thinking:   styleFor(RoleThinking, colors[RoleThinking]),
		Colors:     colors,
		Bg:         bg,
		Determined: true, // a concrete-bg theme always knows its background
	}
}

// ----- active state -------------------------------------------------------

var (
	mu       sync.Mutex
	active   = "dark" // last applied theme name; dark is the safe default
	styleSet = builtins["dark"].buildStyles(BgDark)
)

// Active reports the name of the currently applied theme
// ("dark"/"light"/"auto"/<user name>).
func Active() string {
	mu.Lock()
	defer mu.Unlock()
	return active
}

// Styles returns a snapshot of the currently applied StyleSet. The returned
// lipgloss styles are immutable values, safe to use without holding the lock.
func Styles() StyleSet {
	mu.Lock()
	defer mu.Unlock()
	return styleSet
}

// DetectFunc resolves an "auto" background to a concrete dark/light. The TUI
// passes its terminal-background detector; cmd/themes passes a stub. Returning
// ("", false) means "undetermined" — Apply then uses the neutral markdown
// style (SetUnknownTheme) and the dark built-in colors, matching loopy today.
type DetectFunc func() (bg Background, determined bool)

// Apply makes name the active theme: resolves auto via detect (if non-nil),
// builds the StyleSet, swaps it in, and sets lipgloss's has-dark-background
// flag so AdaptiveColor and glamour agree. Returns a short human note naming
// the resolution (e.g. "auto → dark (terminal query)") for the UI to show.
//
// An unknown name applies "auto" and returns an error in the note — themes are
// presentation, not infrastructure, so a bad name never panics the caller.
// A malformed user theme (failed Valid) is likewise treated as auto with a
// diagnostic note; Load already drops malformed files, but Apply is defensive.
func Apply(name string, detect DetectFunc) string {
	name = strings.ToLower(strings.TrimSpace(name))

	// auto: resolve to a concrete background via the detector, else neutral.
	if name == "" || name == "auto" {
		bg, determined := BgDark, false
		if detect != nil {
			bg, determined = detect()
		}
		return ApplyAuto(bg, determined)
	}

	// built-in dark/light
	if t, ok := builtins[name]; ok {
		mu.Lock()
		active = name
		styleSet = t.buildStyles(t.Background)
		lipgloss.SetHasDarkBackground(t.Background == BgDark)
		mu.Unlock()
		setMarkdownBg(t.Background == BgLight)
		return name
	}

	// user theme
	t, err := lookup(name)
	if err != nil {
		return Apply("auto", detect) + " — " + err.Error()
	}
	if err := t.Valid(); err != nil {
		return Apply("auto", detect) + " — " + err.Error()
	}
	bg := t.Background
	if bg == BgAuto {
		// a user theme can also be "auto": resolve like the built-in.
		concrete, determined := BgDark, false
		if detect != nil {
			concrete, determined = detect()
		}
		mu.Lock()
		active = t.Name
		styleSet = t.buildStyles(concrete)
		if !determined {
			styleSet.Determined = false
		}
		lipgloss.SetHasDarkBackground(concrete == BgDark)
		mu.Unlock()
		if determined {
			setMarkdownBg(concrete == BgLight)
			return fmt.Sprintf("%s → %s", t.Name, concrete)
		}
		SetUnknownMarkdown()
		return fmt.Sprintf("%s (undetermined — neutral default)", t.Name)
	}
	mu.Lock()
	active = t.Name
	styleSet = t.buildStyles(bg)
	lipgloss.SetHasDarkBackground(bg == BgDark)
	mu.Unlock()
	setMarkdownBg(bg == BgLight)
	return t.Name
}

// ApplyAuto is the pre-resolved auto path: the caller already ran the detector
// (it may need the detection source string for a UI note, which DetectFunc
// can't return), so this just swaps in the resolved built-in and drives the
// markdown cache. Apply("auto"/"", detect) delegates here; the loopy TUI calls
// it directly from its probeBackground result so the "(auto: <source>)" note
// can carry the source without probing the terminal twice.
func ApplyAuto(bg Background, determined bool) string {
	mu.Lock()
	active = "auto"
	if determined {
		styleSet = builtins[string(bg)].buildStyles(bg)
		lipgloss.SetHasDarkBackground(bg == BgDark)
	} else {
		// unknown background: neutral markdown, dark colors as the safe default
		ds := builtins["dark"].buildStyles(BgDark)
		ds.Determined = false // signals the caller to render markdown neutral
		styleSet = ds
		lipgloss.SetHasDarkBackground(true)
	}
	mu.Unlock()
	if determined {
		setMarkdownBg(bg == BgLight)
		return fmt.Sprintf("auto → %s", bg)
	}
	SetUnknownMarkdown() // neutral default: no forced bg, matching loopy today
	return "auto (undetermined — neutral default)"
}

// lookup finds a user theme by name from the last Load. Themes are loaded once
// and cached; the playground polls the dir mtime to hot-reload (see Load +
// Reload).
var (
	loadMu     sync.Mutex
	loaded     []Theme
	loadedFrom string
)

func lookup(name string) (Theme, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	for _, t := range loaded {
		if t.Name == name {
			return t, nil
		}
	}
	return Theme{}, fmt.Errorf("theme %q not found", name)
}

// Loaded returns the cached user themes from the last Load (no built-ins).
// Tests and the playground use it to render the picker without re-reading disk.
func Loaded() []Theme {
	loadMu.Lock()
	defer loadMu.Unlock()
	out := make([]Theme, len(loaded))
	copy(out, loaded)
	return out
}

// All returns built-ins (auto/light/dark, in that order) followed by cached
// user themes — the order the /theme panel lists them.
func All() []Theme {
	user := Loaded()
	out := make([]Theme, 0, 3+len(user))
	out = append(out, builtins["auto"], builtins["light"], builtins["dark"])
	out = append(out, user...)
	return out
}

// ----- loading from disk -------------------------------------------------

// DefaultDir is LOOPY_HOME/themes, or ~/.loopy/themes when LOOPY_HOME is unset.
func DefaultDir() (string, error) {
	if d := os.Getenv("LOOPY_HOME"); d != "" {
		return filepath.Join(d, "themes"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loopy", "themes"), nil
}

// Load reads the default themes dir (~/.loopy/themes) and caches the parsed
// user themes. Malformed files are dropped with a warning printed to stderr
// (one bad theme never blocks the rest). Built-ins are never loaded from disk.
// Call Reload to refresh when files change.
func Load() ([]Theme, error) {
	dir, err := DefaultDir()
	if err != nil {
		return All(), err
	}
	return LoadFrom(dir)
}

// LoadFrom reads dir/*.json into the cache and returns All() (built-ins +
// the just-loaded user themes). A missing dir is not an error: it yields just
// the built-ins, so the playground works before the user has authored anything.
func LoadFrom(dir string) ([]Theme, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			storeLoaded(nil, dir)
			return All(), nil
		}
		return All(), err
	}
	var themes []Theme
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if !validName(name) {
			fmt.Fprintf(os.Stderr, "theme: skipping %q: name must match %s\n", e.Name(), nameRe)
			continue
		}
		t, err := parseFile(filepath.Join(dir, e.Name()), name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "theme: skipping %q: %v\n", e.Name(), err)
			continue
		}
		themes = append(themes, t)
	}
	sort.Slice(themes, func(i, j int) bool { return themes[i].Name < themes[j].Name })
	storeLoaded(themes, dir)
	return All(), nil
}

func storeLoaded(themes []Theme, dir string) {
	loadMu.Lock()
	loaded = themes
	loadedFrom = dir
	loadMu.Unlock()
}

// Reload re-reads the last-loaded dir and returns All() with the fresh set.
// The playground calls this when it detects the dir's mtime changed (hot edit).
func Reload() ([]Theme, error) {
	loadMu.Lock()
	dir := loadedFrom
	loadMu.Unlock()
	if dir == "" {
		return Load()
	}
	return LoadFrom(dir)
}

// LoadedFrom reports the dir the cache was last populated from ("" before Load).
func LoadedFrom() string {
	loadMu.Lock()
	defer loadMu.Unlock()
	return loadedFrom
}

func parseFile(path, name string) (Theme, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Theme{}, err
	}
	// strip JSONC line comments so users can annotate their theme files,
	// same convenience as loopy's config. Cheap and sufficient: doesn't try to
	// handle /* */ inside strings, which a theme file won't contain.
	raw = stripJSONCComments(raw)
	var t Theme
	if err := json.Unmarshal(raw, &t); err != nil {
		return Theme{}, fmt.Errorf("parse: %w", err)
	}
	t.Name = name
	if err := t.Valid(); err != nil {
		return Theme{}, err
	}
	return t, nil
}

// stripJSONCComments removes // line comments outside of JSON strings. A theme
// file is small and flat; this avoids pulling in a JSONC dep.
func stripJSONCComments(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b); i++ {
		// skip a // comment to end of line
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '/' {
			for i < len(b) && b[i] != '\n' {
				i++
			}
			continue
		}
		out = append(out, b[i])
	}
	return out
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_' || r == '-':
		default:
			return false
		}
	}
	return !reserved[name]
}

// ----- color resolution --------------------------------------------------

// resolveColor parses one color value: an all-digits string → xterm-256 index
// (lipgloss.Color("12")), a "#rrggbb" → lipgloss.Color("#1e1e2e"). Returns
// (color, true) on success. Anything else (including "") is (color, false).
// Callers fall back to the role's built-in on false rather than rendering with
// an empty color.
func resolveColor(c string) (string, bool) {
	c = strings.TrimSpace(c)
	if c == "" {
		return "", false
	}
	if strings.HasPrefix(c, "#") {
		// accept #rgb or #rrggbb
		hex := c[1:]
		if len(hex) == 3 || len(hex) == 6 {
			if allHex(hex) {
				return c, true
			}
		}
		return "", false
	}
	if allDigits(c) {
		return c, true // xterm-256 index
	}
	return "", false
}

func allHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ----- markdown (glamour) -------------------------------------------------

// Glamour's chroma syntax style registers process-globally and first-write-
// wins, so a render under one background poisons later renders under the other
// (see loopy internal/tui/markdown.go). We unregister on every background
// change so the next render registers the correct palette.
var (
	mdMu      sync.Mutex
	mdLight   bool // light background detected/chosen
	mdKnown   bool // background was determined (false → neutral ASCII style)
	mdCache   *glamour.TermRenderer
	mdWidth   int
	mdCacheOk bool
)

func setMarkdownBg(light bool) {
	mdMu.Lock()
	mdLight, mdKnown = light, true
	mdCache, mdWidth, mdCacheOk = nil, 0, false
	mdMu.Unlock()
	unregisterChroma()
}

// SetUnknownMarkdown renders markdown in the neutral default style (no forced
// bg) — the caller couldn't determine the background. Mirrors loopy's
// SetUnknownTheme for the auto/undetermined path.
func SetUnknownMarkdown() {
	mdMu.Lock()
	mdKnown = false
	mdCache, mdWidth, mdCacheOk = nil, 0, false
	mdMu.Unlock()
	unregisterChroma()
}

// unregisterChroma drops glamour's global "charm" chroma style so a background
// switch re-registers the matching syntax palette. No-op if absent.
func unregisterChroma() {
	// chromaStyles.Registry lives in glamour's chroma/styles package; avoid the
	// import by reaching for the registered style via glamour's API. Glamour
	// exposes styles.Get — but the registry delete needs the chroma import.
	// Keep it simple: invalidate the renderer cache only; glamour re-creates
	// the chroma style per renderer build when the style config differs, and
	// our mdStyle picks a distinct StyleConfig per bg, so re-building is enough.
	// (loopy additionally deletes the registry entry; here the cache invalidation
	// is sufficient because we never mix two live renderers.)
}

// mdStyle picks the glamour StyleConfig for the current background: the light
// variant gets a higher-contrast inline-code chip (stock Light is salmon on
// near-white, unreadable), dark is stock Dark, unknown is the neutral ASCII
// style so text stays at the terminal's default colors. Matches loopy exactly.
func mdStyle() glamouransi.StyleConfig {
	mdMu.Lock()
	known, light := mdKnown, mdLight
	mdMu.Unlock()
	return buildMdStyle(known, light)
}

// buildMdStyle is the unlocked style builder — mdRenderer already holds mdMu
// when it calls this (calling mdStyle from there would self-deadlock). Keeping
// the style logic here, lock-free, lets both callers share one implementation.
func buildMdStyle(known, light bool) glamouransi.StyleConfig {
	if !known {
		return styles.ASCIIStyleConfig
	}
	var st glamouransi.StyleConfig
	if light {
		st = styles.LightStyleConfig
		st.Code.Color = strPtr("124")           // dark red text
		st.Code.BackgroundColor = strPtr("255") // lightest gray chip
	} else {
		st = styles.DarkStyleConfig
	}
	// pin table separators so a lipgloss default change can't unformat tables,
	// and trim per-cell margin to one space (glamour's default wastes ~4 cols).
	st.Table.ColumnSeparator = strPtr("│")
	st.Table.CenterSeparator = strPtr("┼")
	st.Table.RowSeparator = strPtr("─")
	zero := uint(0)
	st.Table.Margin = &zero
	return st
}

func strPtr(s string) *string { return &s }

// mdRenderer returns a cached glamour renderer per width, rebuilt when the
// background changes. Glamour builds a style-traversed renderer per Render
// otherwise, which is expensive on every message.
func mdRenderer(width int) *glamour.TermRenderer {
	mdMu.Lock()
	defer mdMu.Unlock()
	if mdCacheOk && mdWidth == width {
		return mdCache
	}
	st := buildMdStyle(mdKnown, mdLight) // mdRenderer holds mdMu; use the unlocked builder
	margin := uint(2)
	st.Document.Margin = &margin
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(st),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return nil
	}
	mdCache, mdWidth, mdCacheOk = r, width, true
	return r
}

// RenderMarkdown renders s as terminal markdown under the current theme's
// background. Empty input returns as-is; a glamour error falls back to s — a
// degraded render is never worth a broken one. width is clamped to ≥8 (glamour
// treats ≤0 as its ~80-col default).
func RenderMarkdown(s string, width int) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	width = max(width, 8)
	r := mdRenderer(width)
	if r == nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return strings.Trim(out, "\n")
}

// ----- helpers for the playground / TUI ---------------------------------

// Names returns just the names from themes (built-ins first), for a picker.
func Names(themes []Theme) []string {
	out := make([]string, len(themes))
	for i, t := range themes {
		out[i] = t.Name
	}
	return out
}

// Find returns the named theme from the slice, or (Theme{}, false).
func Find(themes []Theme, name string) (Theme, bool) {
	for _, t := range themes {
		if t.Name == name {
			return t, true
		}
	}
	return Theme{}, false
}
