// Command tps-demo is a fake page that revs a simulated token stream and
// renders all four tps gauges live, side by side, so you can pick a vibe.
//
//	go run ./cmd/tps-demo            # interactive: hold SPACE to floor it
//	go run ./cmd/tps-demo -snap      # static frames at a few TPS levels (no TTY)
//
// Hold SPACE to floor the throttle; release to coast. 'a' toggles auto-rev
// (on by default — the engine cycles idle → rev → redline → shift on its own).
// 'r' resets the peak. q / esc / ctrl+c quits.
package main

import (
	"flag"
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/context-labs/loopy/internal/tps"
)

const frame = 60 * time.Millisecond

type frameMsg time.Time

func frameTick() tea.Cmd {
	return tea.Tick(frame, func(t time.Time) tea.Msg { return frameMsg(t) })
}

type model struct {
	tracker *tps.Tracker

	auto    bool   // engine revs on its own
	floor   bool   // SPACE held — wide-open throttle
	cycle   int    // which rev cycle we're in (drives higher peaks each lap)
	phase   string // idle | rev | redline | shift
	phaseT  time.Duration
	target  float64 // current target t/s
	rate    float64 // smoothed actual t/s feeding the tracker
	carry   float64 // fractional tokens carried across frames (int truncation fix)
	maxEver float64

	width, height int
}

func initial() model {
	return model{
		tracker: tps.New(),
		auto:    true,
		phase:   "idle",
	}
}

func (m model) Init() tea.Cmd { return frameTick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, frameTick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case " ":
			m.floor = true
		case " " + "release": // not produced; handled in release below
		case "a":
			m.auto = !m.auto
		case "r":
			m.tracker.Reset()
			m.maxEver = 0
			m.cycle = 0
			m.phase = "idle"
			m.phaseT = 0
		}

	case frameMsg:
		m = m.step(frame)
		m.tracker.Sample()
		return m, frameTick()
	}
	return m, nil
}

// step advances the simulated engine one frame: pick a target rate from the
// current phase, smooth toward it, emit that many tokens, and run the phase
// machine that cycles idle → rev → redline → shift.
func (m model) step(dt time.Duration) model {
	m.phaseT += dt

	// base target from the phase machine (auto-rev). SPACE overrides to
	// wide-open throttle; letting go snaps to a coast.
	base := 6.0 // idle
	switch m.phase {
	case "idle":
		base = 6 + 3*math.Sin(float64(m.phaseT)/400e6)
		if m.phaseT > 1500*time.Millisecond {
			m.phase, m.phaseT = "rev", 0
		}
	case "rev":
		// ramp from idle up toward this cycle's peak
		peak := 90 + float64(m.cycle)*18
		prog := clamp01(float64(m.phaseT) / float64(3*time.Second))
		base = 6 + (peak-6)*easeIn(prog) + jitter(m.phaseT, 4)
		if m.phaseT > 3*time.Second {
			m.phase, m.phaseT = "redline", 0
		}
	case "redline":
		peak := 90 + float64(m.cycle)*18
		base = peak - 8 + jitter(m.phaseT, 10) // hover near the top, wobbling
		if m.phaseT > 1800*time.Millisecond {
			m.phase, m.phaseT = "shift", 0
		}
	case "shift":
		base = 35 + jitter(m.phaseT, 6) // dip after the "gear change"
		if m.phaseT > 900*time.Millisecond {
			m.cycle++
			m.phase, m.phaseT = "idle", 0
		}
	}

	if m.floor {
		base = 140 // floor it regardless of phase
	} else if !m.auto {
		base = 4 // manual + no gas = idle
	}

	// smooth the actual rate toward the target so the needle sweeps instead
	// of teleporting (feels like a real tach's needle inertia).
	m.rate += (base - m.rate) * 0.25

	// emit tokens for this frame at the smoothed rate; carry the fractional
	// remainder so a slow stream (rate*dt < 1 token) still produces arrivals
	// instead of truncating to zero every frame.
	m.carry += m.rate * dt.Seconds()
	tokens := int(m.carry)
	m.carry -= float64(tokens)
	if tokens < 0 {
		tokens = 0
	}
	m.tracker.AddTokens(tokens)
	if m.rate > m.maxEver {
		m.maxEver = m.rate
	}
	return m
}

func (m model) View() string {
	snap := m.tracker.Snapshot()

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13")).Render("LOOPY · TPS GAUGE LAB")
	sub := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("4 ways to feel the tokens rip")
	pad := strings.Repeat(" ", max(0, m.width-len(title)-len(sub)-2))
	top := title + pad + sub

	// a full-width status line using each gauge, the way the real TUI would
	status := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render
	cwd := " ~/workspace/loopy"
	mdl := "opus-420 (high)"
	prov := "openrouter"

	mkPanel := func(label, hint, gauge string) string {
		head := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(label) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("  "+hint)
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238")).
			Padding(0, 1).
			Render(gauge)
		return head + "\n" + box
	}

	// render the gauge once per panel; render in the loopy status-line context
	// too (cwd · model · provider · gauge) so you can see how each reads inline.
	inline := func(g string) string {
		return status(fmt.Sprintf(" %s   %s   %s   %s", cwd, mdl, prov, stripANSI(g))) + "   " + g
	}

	bar := mkPanel("① bar", "fill + leading edge, green→red", tps.RenderBar(snap))
	tach := mkPanel("② tach", "analog needle dial, redline zone", tps.RenderTach(snap))
	spark := mkPanel("③ sparkline", "rolling waveform, newest lit", tps.RenderSparkline(snap))
	lights := mkPanel("④ shift lights", "F1 LEDs, blink at redline", tps.RenderShiftLights(snap))

	panels := lipgloss.JoinVertical(lipgloss.Left, bar, "", tach, "", spark, "", lights)

	// live status-line previews, one per gauge
	statuses := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("── status-line preview (each gauge inlined) ──") + "\n" +
		inline(tps.RenderBar(snap)) + "\n" +
		inline(tps.RenderTach(snap)) + "\n" +
		inline(tps.RenderSparkline(snap)) + "\n" +
		inline(tps.RenderShiftLights(snap))

	telemetry := status(fmt.Sprintf(
		"phase %-8s cycle %d   target %5.1f → rate %5.1f t/s   peak %5.1f   redline %5.0f   frame %d",
		m.phase, m.cycle, m.target, m.rate, snap.Peak, snap.Redline, snap.Frame,
	))
	if m.floor {
		telemetry = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Render("│ FLOORED │ ") + telemetry
	}
	if !m.auto {
		telemetry = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("MANUAL ") + telemetry
	}

	controls := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(
		"hold SPACE floor it · a auto-rev: " + boolStr(m.auto) + " · r reset peak · q quit")

	body := lipgloss.JoinVertical(lipgloss.Left,
		top, "",
		panels, "",
		statuses, "",
		telemetry, "",
		controls,
	)
	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, body)
}

// ── helpers ────────────────────────────────────────────────────────────────

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func easeIn(v float64) float64 { return v * v }

func jitter(t time.Duration, amp float64) float64 {
	return amp * math.Sin(float64(t)/90e6)
}

func boolStr(b bool) string {
	if b {
		return "on "
	}
	return "off"
}

// fakeClock is a controllable time source for the snapshot mode (no TTY), so
// the tracker's sliding-window TPS reads deterministically from spread events.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// stripANSI removes escape sequences so the inline preview keeps its dim style
// while the colored gauge renders beside it.
func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '\x1b' {
			in = true
			continue
		}
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func main() {
	snap := flag.Bool("snap", false, "render static frames at several TPS levels instead of the live TUI")
	rec := flag.Bool("rec", false, "record N ASCII frames of the animation to stdout (no TTY) for headless verification")
	flag.Parse()
	switch {
	case *snap:
		runSnapshot()
		return
	case *rec:
		runRecording()
		return
	}
	if _, err := tea.NewProgram(initial(), tea.WithAltScreen()).Run(); err != nil {
		fmt.Println("error:", err)
	}
}

// runRecording animates the engine headlessly and prints one compact frame
// per tick to stdout, so the revving motion is verifiable without a TTY. Each
// frame shows all four gauges plus the phase telemetry, so you can watch the
// needle sweep, the sparkline fill, and the shift lights climb into RED!.
func runRecording() {
	m := initial()
	m.width, m.height = 96, 40
	const frames = 90 // ~5.4s of animation at 60ms/frame
	for i := 0; i < frames; i++ {
		m = m.step(frame)
		m.tracker.Sample()
		fmt.Print("\x1b[H\x1b[J") // clear to top-left so frames overwrite
		fmt.Println(compactFrame(m))
		time.Sleep(frame)
	}
}

// compactFrame is a single-line-per-gauge rendering for the recording mode —
// the full TUI View's box layout is overkill for a scrolling ASCII capture.
func compactFrame(m model) string {
	s := m.tracker.Snapshot()
	head := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13")).Render(
		fmt.Sprintf("LOOPY TPS · frame %d · phase %-8s rate %5.1f t/s peak %5.1f redline %5.0f",
			s.Frame, m.phase, m.rate, s.Peak, s.Redline))
	status := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(fmt.Sprintf(
		"cwd ~/loopy   opus-420 (high)   openrouter   "))
	return head + "\n" +
		fmt.Sprintf("① bar          %s%s\n", status, tps.RenderBar(s)) +
		fmt.Sprintf("② tach         %s%s\n", status, tps.RenderTach(s)) +
		fmt.Sprintf("③ sparkline    %s%s\n", status, tps.RenderSparkline(s)) +
		fmt.Sprintf("④ shift lights %s%s", status, tps.RenderShiftLights(s))
}
// visuals are viewable without an interactive terminal — handy for a quick
// look or a screenshot. Each level feeds the tracker long enough for the
// redline to settle, then prints the gauges in a compact grid.
func runSnapshot() {
	levels := []struct {
		name string
		tps  int
	}{
		{"idle (~5 t/s)", 5},
		{"cruising (~40 t/s)", 40},
		{"ripping (~80 t/s)", 80},
		{"redline (~140 t/s)", 140},
	}
	pr := lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	lab := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	fmt.Println(pr.Render("LOOPY · TPS GAUGE LAB") + "  " + dim.Render("— four vibes, four load points"))
	fmt.Println(dim.Render("(run with no flags for the live revving TUI: hold SPACE to floor it)"))
	fmt.Println()

	for _, lv := range levels {
		clock := &fakeClock{t: time.UnixMilli(0)}
		tr := tps.New(tps.WithNow(clock.now))
		// spread lv.tps tokens evenly across a 1s window so TPS reads ~lv.tps
		// and the redline/peak settle; advancing the clock per Add keeps events
		// from collapsing to one instant (which would peg span≈0 → huge TPS).
		for i := 0; i < lv.tps; i++ {
			tr.AddTokens(1)
			clock.t = clock.t.Add(time.Second / time.Duration(lv.tps))
		}
		// sample a few times so sparkline + shift-light history has shape
		for i := 0; i < 8; i++ {
			tr.Sample()
		}
		s := tr.Snapshot()
		fmt.Println(lab.Render(lv.name))
		fmt.Printf("  ① bar         %s\n", tps.RenderBar(s))
		fmt.Printf("  ② tach        %s\n", tps.RenderTach(s))
		fmt.Printf("  ③ sparkline   %s\n", tps.RenderSparkline(s))
		fmt.Printf("  ④ shift lights %s\n", tps.RenderShiftLights(s))
		fmt.Println()
	}
}
