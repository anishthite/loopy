// Command themes is a standalone playground for the internal/theme package:
// a fake loopy-style transcript you can recolor by picking a theme, so you can
// author a theme JSON file and see it applied live without running the full TUI.
//
// Usage:
//
//	themes                 # use ~/.loopy/themes
//	themes --dir ./examples  # use a different themes dir (e.g. the shipped ones)
//
// Keys:
//
//	↑/↓ or j/k   move the theme cursor
//	enter / ←/→  apply the highlighted theme (re-renders the transcript live)
//	t            open/close the theme picker
//	r            force a hot-reload of the themes dir (also auto-reloads on edit)
//	q or ctrl+c  quit
//
// Edit a JSON file in the themes dir (or drop in a new one) and the list
// refreshes automatically — the dir's mtime is polled, no file watcher dep.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/context-labs/loopy/internal/theme"
)

func main() {
	dir := flag.String("dir", "", "themes directory (default ~/.loopy/themes)")
	flag.Parse()

	themesDir := *dir
	if themesDir == "" {
		d, err := theme.DefaultDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "themes:", err)
			os.Exit(1)
		}
		themesDir = d
	}

	if _, err := theme.LoadFrom(themesDir); err != nil {
		// a missing dir is fine (LoadFrom yields built-ins); anything else is fatal
		fmt.Fprintf(os.Stderr, "themes: load %s: %v\n", themesDir, err)
		os.Exit(1)
	}

	// Start in dark so the first frame is deterministic; the user can flip live.
	theme.Apply("dark", nil)

	p := tea.NewProgram(newModel(themesDir), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "themes:", err)
		os.Exit(1)
	}
}

// ----- model -------------------------------------------------------------

type model struct {
	dir      string         // themes dir being watched
	themes   []theme.Theme  // current list (built-ins first)
	cursor   int            // highlighted row in the picker
	picker   bool           // picker open?
	viewport viewport.Model // scrollable transcript
	width    int
	height   int
	reloadIn time.Duration // backed-off poll interval
	lastNote string        // last apply note, shown briefly in the footer
	noteAt   time.Time
}

type tickMsg time.Time

func newModel(dir string) *model {
	m := &model{
		dir:      dir,
		themes:   theme.All(),
		cursor:   2, // "dark" is index 2; start highlighted there
		picker:   true,
		reloadIn: 800 * time.Millisecond,
	}
	m.viewport = viewport.New(80, 20)
	m.viewport.SetContent(m.transcript())
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(tick(m.reloadIn), waitForDirChange(m.dir))
}

// tick drives the brief footer-note timeout (the "applied: …" line fades).
func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// dirChangedMsg signals the themes dir's mtime changed → reload the list.
type dirChangedMsg struct{ changed bool }

// waitForDirChange polls the dir mtime on a backoff so a hot edit shows up
// without a fsnotify dependency. Re-schedules itself each tick.
func waitForDirChange(dir string) tea.Cmd {
	return func() tea.Msg {
		info, err := os.Stat(dir)
		if err != nil {
			return dirChangedMsg{changed: false}
		}
		_ = info
		// We don't track the last mtime across process boundaries here; instead
		// poll on a short cadence and let reload be idempotent (LoadFrom re-reads
		// every time; Apply on an already-active theme is a no-op re-render).
		return dirChangedMsg{changed: dirMightHaveChanged(dir)}
	}
}

var dirMtimes = map[string]time.Time{}

// dirMightHaveChanged compares the dir + its json files' mtimes against the
// last-seen snapshot. First call seeds the snapshot and returns false.
func dirMightHaveChanged(dir string) bool {
	cur := scanMtimes(dir)
	prev := dirMtimes[dir]
	dirMtimes[dir] = cur
	if prev.IsZero() {
		return false
	}
	return !prev.Equal(cur)
}

func scanMtimes(dir string) time.Time {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}
	}
	latest := time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

// ----- update ------------------------------------------------------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tickMsg:
		// clear the footer note after it has shown for a bit
		if !m.noteAt.IsZero() && time.Since(m.noteAt) > 1500*time.Millisecond {
			m.lastNote = ""
		}
		return m, tick(m.reloadIn)

	case dirChangedMsg:
		cmds := []tea.Cmd{waitForDirChange(m.dir)}
		if msg.changed {
			// a file was added/edited: reload and keep the cursor valid
			prev := m.activeName()
			m.themes = reloadThemes(m.dir)
			m.cursor = clamp(m.cursor, 0, len(m.themes)-1)
			// if the active theme still exists, re-apply it so an edit to the
			// active file shows up immediately in the transcript
			if _, ok := theme.Find(m.themes, prev); ok && prev == theme.Active() {
				note := theme.Apply(prev, nil)
				m.setViewport()
				m.flash(note)
			}
		}
		return m, tea.Batch(cmds...)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - footerHeight(m)
		m.setViewport()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "t":
			m.picker = !m.picker
			return m, nil
		case "r":
			m.themes = reloadThemes(m.dir)
			m.cursor = clamp(m.cursor, 0, len(m.themes)-1)
			note := theme.Apply(m.themes[m.cursor].Name, nil)
			m.setViewport()
			m.flash(note)
			return m, nil
		}

		if !m.picker {
			// when the picker is closed, scroll keys still work on the transcript
			switch msg.String() {
			case "up", "k":
				m.viewport.LineUp(1)
			case "down", "j":
				m.viewport.LineDown(1)
			case "pgup":
				m.viewport.HalfViewUp()
			case "pgdown":
				m.viewport.HalfViewDown()
			}
			return m, nil
		}

		switch msg.String() {
		case "up", "k":
			m.cursor = (m.cursor - 1 + len(m.themes)) % len(m.themes)
		case "down", "j":
			m.cursor = (m.cursor + 1) % len(m.themes)
		case "enter", "left", "right":
			note := theme.Apply(m.themes[m.cursor].Name, nil)
			m.setViewport()
			m.flash(note)
		}
		return m, nil
	}
	return m, nil
}

// reloadThemes re-reads the dir and returns All(); errors fall back to builtins.
func reloadThemes(dir string) []theme.Theme {
	all, err := theme.LoadFrom(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "themes: reload %s: %v\n", dir, err)
		return theme.All()
	}
	return all
}

func (m *model) activeName() string {
	if m.cursor < len(m.themes) {
		return m.themes[m.cursor].Name
	}
	return ""
}

func (m *model) setViewport() {
	m.viewport.SetContent(m.transcript())
}

func (m *model) flash(note string) {
	m.lastNote = note
	m.noteAt = time.Now()
}

func footerHeight(m *model) int { return 2 } // picker line + status line

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// ----- view --------------------------------------------------------------

func (m *model) View() string {
	st := theme.Styles()
	var b strings.Builder

	// transcript viewport fills all but the bottom two lines
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// theme picker row
	b.WriteString(m.pickerView(st))
	b.WriteString("\n")

	// status / footer
	b.WriteString(m.statusView(st))
	return b.String()
}

func (m *model) pickerView(st theme.StyleSet) string {
	if !m.picker {
		return st.Dim.Render("  t: open theme picker   ↑↓ move   enter apply   r reload   q quit")
	}
	var cells []string
	for i, t := range m.themes {
		label := t.Name
		// show the theme's "you" color as a swatch in the chip, so the picker
		// itself previews each theme's palette at a glance
		swatch := "●"
		if c, ok := st.Colors[theme.RoleYou]; ok {
			swatch = lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render("●")
		}
		cell := fmt.Sprintf("%s %s", swatch, label)
		if i == m.cursor {
			cell = lipgloss.NewStyle().Bold(true).Reverse(true).Render(cell)
		} else {
			cell = st.Dim.Render(cell)
		}
		cells = append(cells, cell)
	}
	return "  " + strings.Join(cells, "  ")
}

func (m *model) statusView(st theme.StyleSet) string {
	active := theme.Active()
	bg := st.Bg
	dir := m.dir
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		dir = st.Error.Render(dir + " (missing — showing built-ins)")
	}
	status := st.Dim.Render(fmt.Sprintf("  dir: %s   active: %s   bg: %s", dir, active, bg))
	if m.lastNote != "" {
		status = st.Dim.Render("  applied: ") + st.You.Render(m.lastNote)
	}
	return status
}

// transcript builds a fake loopy-style chat under the current theme: a user
// turn, an assistant turn with markdown + reasoning, a tool call, and an error
// note. Every colored element reads theme.Styles() so a switch re-renders it.
func (m *model) transcript() string {
	st := theme.Styles()
	width := m.viewport.Width
	if width < 8 {
		width = 80
	}
	contentWidth := width - 4 // ┃ " prompt + margin

	var b strings.Builder

	// user turn
	b.WriteString(st.You.Render("You"))
	b.WriteString("\n")
	b.WriteString(wrapPlain("How do I read a file in Go and handle missing-path errors cleanly?", contentWidth))
	b.WriteString("\n\n")

	// reasoning tokens
	b.WriteString(st.Thinking.Render("thinking"))
	b.WriteString("\n")
	b.WriteString(st.Thinking.Render(wrapPlain("The user wants idiomatic file reading with a typed error. os.ReadFile plus fs.ErrExist wrapping via errors.Is is the modern shape.", contentWidth)))
	b.WriteString("\n\n")

	// assistant turn (markdown)
	b.WriteString(st.Bot.Render("Assistant"))
	b.WriteString("\n")
	md := theme.RenderMarkdown(`Use **os.ReadFile** for the whole file in one call, and wrap with **errors.Is** to test for a missing path:

`+"```go"+`
data, err := os.ReadFile(path)
if err != nil {
    if errors.Is(err, fs.ErrNotExist) {
        return fmt.Errorf("config %q not found: %w", path, err)
    }
    return err
}
`+"```"+`

| call | returns | notes |
|---|---|---|
| `+"`ReadFile`"+` | `+"`[]byte, error`"+` | whole file, no fd to close |
| `+"`Open`"+` | `+"`*File, error`"+` | stream large files |`, contentWidth)
	b.WriteString(md)
	b.WriteString("\n\n")

	// tool call
	b.WriteString(st.Tool.Render("⚒ bash"))
	b.WriteString(" ")
	b.WriteString(st.Dim.Render("cat config.json"))
	b.WriteString("\n")
	b.WriteString(st.Dim.Render(indent(`{ "defaultModel": "..." }`, "  ")))
	b.WriteString("\n\n")

	// error note
	b.WriteString(st.Error.Render("✗ config save failed: disk full"))
	b.WriteString("\n")

	return b.String()
}

func wrapPlain(s string, width int) string {
	if width < 1 {
		return s
	}
	words := strings.Fields(s)
	var b strings.Builder
	col := 0
	for i, w := range words {
		if i == 0 {
			b.WriteString(w)
			col = len(w)
			continue
		}
		if col+1+len(w) > width {
			b.WriteString("\n")
			b.WriteString(w)
			col = len(w)
		} else {
			b.WriteString(" ")
			b.WriteString(w)
			col += 1 + len(w)
		}
	}
	return b.String()
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
