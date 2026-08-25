package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetState restores active theme + caches between tests so they don't leak.
func resetState(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		mu.Lock()
		active = "dark"
		styleSet = builtins["dark"].buildStyles(BgDark)
		mu.Unlock()
		mdMu.Lock()
		mdLight, mdKnown = false, true
		mdCache, mdWidth, mdCacheOk = nil, 0, false
		mdMu.Unlock()
		loadMu.Lock()
		loaded, loadedFrom = nil, ""
		loadMu.Unlock()
	})
}

// writeFile writes content into a temp themes dir and returns the dir path.
func writeThemesDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		mustWrite(t, filepath.Join(dir, name), body)
	}
	return dir
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ----- built-in exactness: the correctness anchor -------------------------

// The built-ins carry the exact xterm-256 indices loopy's TUI hardcodes today,
// so switching to dark/light/auto emits byte-identical SGR. Any drift here
// would break loopy's contrast tests when the TUI wires onto this package.
func TestBuiltinsCarryExactIndices(t *testing.T) {
	wantDark := map[Role]string{
		RoleYou: "12", RoleBot: "13", RoleTool: "11",
		RoleDim: "245", RoleError: "9", RoleThinking: "245",
	}
	wantLight := map[Role]string{
		RoleYou: "21", RoleBot: "90", RoleTool: "136",
		RoleDim: "240", RoleError: "124", RoleThinking: "240",
	}
	for role, c := range wantDark {
		if got := builtins["dark"].Colors[role]; got != c {
			t.Errorf("dark %s = %q, want %q", role, got, c)
		}
	}
	for role, c := range wantLight {
		if got := builtins["light"].Colors[role]; got != c {
			t.Errorf("light %s = %q, want %q", role, got, c)
		}
	}
	if _, ok := builtins["auto"].Colors[RoleYou]; ok {
		t.Error("auto should have no Colors (sentinel, resolved by detector)")
	}
}

// Applying dark then light produces the same body/code colors loopy's contrast
// tests assert (234/252/251/124/255) — proving Apply doesn't drift the built-ins
// and that markdown follows the background.
func TestApplyBuiltinsRenderExactMarkdown(t *testing.T) {
	resetState(t)
	Apply("light", nil)
	out := RenderMarkdown("plain body text", 60)
	if !strings.Contains(out, "\x1b[38;5;234m") {
		t.Errorf("light body should be 234: %q", out)
	}
	out = RenderMarkdown("use `config.Save` here", 60)
	if !strings.Contains(out, "48;5;255") || !strings.Contains(out, "38;5;124") {
		t.Errorf("light inline code should be 124 on 255 chip: %q", out)
	}

	Apply("dark", nil)
	out = RenderMarkdown("plain body text", 60)
	if !strings.Contains(out, "\x1b[38;5;252m") {
		t.Errorf("dark body should be 252: %q", out)
	}
	out = RenderMarkdown("```go\nx := 1\n```", 70)
	if !strings.Contains(out, "38;5;252") || !strings.Contains(out, "38;5;251") {
		t.Errorf("dark body/code should be 252/251: %q", out)
	}
}

// Applying a built-in sets Active() and the StyleSet colors to that built-in.
func TestApplyBuiltinSetsActiveAndStyles(t *testing.T) {
	resetState(t)
	Apply("light", nil)
	if got := Active(); got != "light" {
		t.Fatalf("Active = %q, want light", got)
	}
	if got := Styles().Colors[RoleYou]; got != "21" {
		t.Errorf("light you color = %q, want 21", got)
	}
	Apply("dark", nil)
	if got := Active(); got != "dark" {
		t.Fatalf("Active = %q, want dark", got)
	}
	if got := Styles().Colors[RoleYou]; got != "12" {
		t.Errorf("dark you color = %q, want 12", got)
	}
}

// ----- auto + detector ---------------------------------------------------

// auto with a detector that determines dark applies dark; light applies light.
func TestApplyAutoResolvesViaDetector(t *testing.T) {
	resetState(t)
	note := Apply("auto", func() (Background, bool) { return BgDark, true })
	if !strings.Contains(note, "auto → dark") {
		t.Fatalf("note = %q", note)
	}
	if got := Styles().Bg; got != BgDark {
		t.Errorf("auto→dark Bg = %q", got)
	}
	note = Apply("auto", func() (Background, bool) { return BgLight, true })
	if !strings.Contains(note, "auto → light") {
		t.Fatalf("note = %q", note)
	}
	if got := Styles().Bg; got != BgLight {
		t.Errorf("auto→light Bg = %q", got)
	}
}

// auto with an undetermined detector keeps markdown neutral (no forced body
// color) and Active() == "auto".
func TestApplyAutoUndeterminedIsNeutral(t *testing.T) {
	resetState(t)
	Apply("auto", func() (Background, bool) { return "", false })
	SetUnknownMarkdown() // Apply's undetermined path also calls this
	out := RenderMarkdown("plain body text", 60)
	if strings.Contains(out, "38;5;252") || strings.Contains(out, "38;5;234") {
		t.Errorf("undetermined should not force a body color: %q", out)
	}
	if got := Active(); got != "auto" {
		t.Errorf("Active = %q, want auto", got)
	}
}

// ----- loading from disk -------------------------------------------------

// A valid theme file loads, sorts after built-ins, and applies its colors.
func TestLoadValidTheme(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"nord.json": `{"background":"dark","colors":{"you":"110","bot":"111","dim":"240"}}`,
	})
	all, err := LoadFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := Names(all)
	want := []string{"auto", "light", "dark", "nord"}
	if len(names) != len(want) || names[3] != "nord" {
		t.Fatalf("names = %v, want %v", names, want)
	}
	Apply("nord", nil)
	if got := Styles().Colors[RoleYou]; got != "110" {
		t.Errorf("nord you = %q, want 110", got)
	}
	if got := Styles().Colors[RoleBot]; got != "111" {
		t.Errorf("nord bot = %q, want 111", got)
	}
}

// A missing role falls back to the built-in for the theme's background.
func TestLoadMissingRoleFallsBackToBuiltin(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"minimal.json": `{"background":"dark","colors":{"you":"99"}}`,
	})
	LoadFrom(dir)
	Apply("minimal", nil)
	if got := Styles().Colors[RoleYou]; got != "99" {
		t.Errorf("overridden you = %q, want 99", got)
	}
	// bot not overridden → dark built-in's 13
	if got := Styles().Colors[RoleBot]; got != "13" {
		t.Errorf("fallback bot = %q, want 13 (dark builtin)", got)
	}
	// a light-bg theme falls back to the light table
	dir2 := writeThemesDir(t, map[string]string{
		"minlight.json": `{"background":"light","colors":{"you":"99"}}`,
	})
	LoadFrom(dir2)
	Apply("minlight", nil)
	if got := Styles().Colors[RoleBot]; got != "90" {
		t.Errorf("light fallback bot = %q, want 90 (light builtin)", got)
	}
}

// #hex colors resolve and are stored verbatim. (We assert on the stored color,
// not a rendered SGR: lipgloss's color profile is no-color under `go test` with
// no TTY, so Render emits no escape; in a real terminal it auto-detects
// truecolor and emits 38;2;r;g;b. The stored color is what flows to the
// renderer, so it's the right thing to pin.)
func TestLoadHexColor(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"hex.json": `{"background":"dark","colors":{"you":"#1e1e2e","bot":"#fab387"}}`,
	})
	LoadFrom(dir)
	Apply("hex", nil)
	if got := Styles().Colors[RoleYou]; got != "#1e1e2e" {
		t.Errorf("hex you = %q, want #1e1e2e", got)
	}
	if got := Styles().Colors[RoleBot]; got != "#fab387" {
		t.Errorf("hex bot = %q, want #fab387", got)
	}
	// a 3-digit hex also resolves
	dir3 := writeThemesDir(t, map[string]string{
		"hex3.json": `{"background":"dark","colors":{"you":"#abc"}}`,
	})
	LoadFrom(dir3)
	Apply("hex3", nil)
	if got := Styles().Colors[RoleYou]; got != "#abc" {
		t.Errorf("hex3 you = %q, want #abc", got)
	}
}

// A malformed file is dropped (not fatal) and the rest still load.
func TestLoadMalformedFileDropped(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"good.json":     `{"background":"dark","colors":{"you":"10"}}`,
		"bad.json":      `{not valid json`,
		"badbg.json":    `{"background":"purple","colors":{}}`,
		"badrole.json":  `{"background":"dark","colors":{"erroor":"9"}}`,
		"badcolor.json": `{"background":"dark","colors":{"you":"notacolor"}}`,
	})
	all, _ := LoadFrom(dir)
	names := Names(all)
	// only auto/light/dark/good should survive
	if len(names) != 4 || names[3] != "good" {
		t.Fatalf("expected only good + builtins, got %v", names)
	}
}

// A reserved name (dark.json) is rejected so a user file can't shadow
// builtins: the built-in "dark" still appears exactly once, and the invalid
// "UPPER" never does. The stderr log confirms the files were skipped.
func TestLoadRejectsReservedName(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"dark.json":  `{"background":"dark","colors":{"you":"99"}}`,
		"UPPER.json": `{"background":"dark","colors":{"you":"99"}}`,
	})
	all, _ := LoadFrom(dir)
	names := Names(all)
	// exactly the three built-ins — no user themes survived
	if len(names) != 3 {
		t.Fatalf("reserved/invalid files should be dropped; got %v", names)
	}
	darkCount := 0
	for _, n := range names {
		if n == "dark" {
			darkCount++
		}
		if n == "UPPER" {
			t.Error("invalid name UPPER loaded")
		}
	}
	if darkCount != 1 {
		t.Errorf("built-in dark should appear once, got %d (%v)", darkCount, names)
	}
}

// JSONC line comments are stripped before parse, like loopy's config.
func TestLoadStripsJSONCComments(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"commented.json": `{
  // my nord-ish theme
  "background": "dark",
  "colors": {
    "you": "110", // speaker
    "dim": "240"
  }
}`,
	})
	all, _ := LoadFrom(dir)
	if _, ok := Find(all, "commented"); !ok {
		t.Fatal("commented theme should load after stripping // comments")
	}
}

// A missing themes dir is not an error: just built-ins come back.
func TestLoadMissingDirYieldsBuiltins(t *testing.T) {
	resetState(t)
	all, err := LoadFrom(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("missing dir → 3 builtins, got %d (%v)", len(all), Names(all))
	}
}

// Reload picks up a file added after the first Load (hot-edit support).
func TestReloadSeesNewFile(t *testing.T) {
	resetState(t)
	dir := writeThemesDir(t, map[string]string{
		"a.json": `{"background":"dark","colors":{"you":"10"}}`,
	})
	first, _ := LoadFrom(dir)
	if len(first) != 4 { // 3 builtins + a
		t.Fatalf("first load = %v", Names(first))
	}
	// add a second theme after the initial load
	mustWrite(t, filepath.Join(dir, "b.json"), `{"background":"dark","colors":{"you":"20"}}`)
	second, _ := Reload()
	if len(second) != 5 { // 3 builtins + a + b
		t.Fatalf("reload should see b: %v", Names(second))
	}
}

// Applying an unknown name falls back to auto with a diagnostic note.
func TestApplyUnknownNameFallsBack(t *testing.T) {
	resetState(t)
	note := Apply("nope", nil)
	if !strings.Contains(note, "not found") {
		t.Errorf("unknown name note should mention not found: %q", note)
	}
	if got := Active(); got != "auto" {
		t.Errorf("unknown name → Active = %q, want auto", got)
	}
}

// ----- color resolution --------------------------------------------------

func TestResolveColor(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string
	}{
		{"12", true, "12"},
		{"245", true, "245"},
		{"#1e1e2e", true, "#1e1e2e"},
		{"#abc", true, "#abc"},
		{"", false, ""},
		{"notacolor", false, ""},
		{"#gggggg", false, ""},
		{"#1234", false, ""}, // wrong length
		{" 12 ", true, "12"}, // trimmed
	}
	for _, c := range cases {
		got, ok := resolveColor(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("resolveColor(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ----- Valid -------------------------------------------------------------

func TestThemeValid(t *testing.T) {
	cases := []struct {
		name string
		t    Theme
		ok   bool
	}{
		{"good dark", Theme{Name: "x", Background: BgDark, Colors: map[Role]string{RoleYou: "12"}}, true},
		{"good light", Theme{Name: "x", Background: BgLight, Colors: map[Role]string{RoleYou: "#fff"}}, true},
		{"good auto empty", Theme{Name: "x", Background: BgAuto}, true},
		{"bad bg", Theme{Name: "x", Background: "purple"}, false},
		{"bad role", Theme{Name: "x", Background: BgDark, Colors: map[Role]string{"erroor": "9"}}, false},
		{"bad color", Theme{Name: "x", Background: BgDark, Colors: map[Role]string{RoleYou: "nope"}}, false},
	}
	for _, c := range cases {
		err := c.t.Valid()
		if (err == nil) != c.ok {
			t.Errorf("%s: Valid ok=%v want %v (err=%v)", c.name, err == nil, c.ok, err)
		}
	}
}

// ----- role attributes are fixed ------------------------------------------

// you/bot are bold; thinking is italic; a theme can't drop these by omitting.
func TestRoleAttributesFixed(t *testing.T) {
	resetState(t)
	Apply("dark", nil)
	if !Styles().You.GetBold() {
		t.Error("you should be bold")
	}
	if !Styles().Bot.GetBold() {
		t.Error("bot should be bold")
	}
	if !Styles().Thinking.GetItalic() {
		t.Error("thinking should be italic")
	}
	// a user theme omitting roles still gets the attributes via fallback
	dir := writeThemesDir(t, map[string]string{
		"m.json": `{"background":"dark","colors":{"you":"99"}}`,
	})
	LoadFrom(dir)
	Apply("m", nil)
	if !Styles().You.GetBold() {
		t.Error("user theme you should still be bold")
	}
}
