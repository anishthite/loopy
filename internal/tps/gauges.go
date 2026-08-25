// Package tps gauge renderers. Each Render takes a Snapshot and returns a
// fixed-width ANSI-styled string suitable for a one-line status bar. Colors
// are 256-color escape codes (not lipgloss) so the renderers work standalone
// in cmd/tps-demo and can be re-wrapped by the TUI.
package tps

import (
	"fmt"
	"strings"
)

// Color grade for throughput: idle/green → working/yellow → peak/red. The
// thresholds are fractions of the redline, so the gauge's mood tracks the
// engine rather than absolute token counts. Returns a direct xterm-256 color
// index (46 green, 220 yellow, 196 red) plus a label.
func grade(frac float64) (idx int, word string) {
	switch {
	case frac < 0.33:
		return 46, "green" // bright green
	case frac < 0.66:
		return 220, "yellow" // gold
	default:
		return 196, "red" // bright red
	}
}

// c256 wraps a string in a 256-color foreground by direct index. Using the
// canonical xterm-256 indices (46/220/196/231/240…) keeps colors exact across
// terminals instead of approximating via the RGB cube.
func c256(idx int, s string) string {
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", idx, s)
}

// clampF pins a value to [lo, hi].
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── gauge 1: bar ───────────────────────────────────────────────────────────

const barWidth = 14

// barRunes are the 8 partial-fill steps of one cell, from empty to full, using
// the standard Block Element progress glyphs.
var barRunes = []string{" ", "▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}

// RenderBar draws a horizontal fill bar: green when cruising, yellow under
// load, red at the top. A trailing "❯" marks the leading edge and a numeric
// TPS readout rides the right.
func RenderBar(s Snapshot) string {
	frac := clampF(s.TPS/s.Redline, 0, 1)
	idx, _ := grade(frac)
	total := float64(barWidth) * frac
	full := int(total)
	part := int((total - float64(full)) * 8) // 0..7
	var bb strings.Builder
	for i := 0; i < barWidth; i++ {
		if i < full {
			bb.WriteString("█")
		} else if i == full && part > 0 {
			bb.WriteString(barRunes[part])
		} else {
			bb.WriteString(" ")
		}
	}
	bar := c256(idx, bb.String())
	lead := c256(idx, "❯")
	return fmt.Sprintf("%s%s %s%4.0ft/s%s", bar, lead, dim, s.TPS, reset)
}

// ── gauge 2: tach ──────────────────────────────────────────────────────────

// RenderTach draws an analog tachometer: a 180° arc of tick marks with a
// sweeping needle. The last 20% of the arc is the redline zone. The needle
// sits at frac of the sweep; tick marks below the needle are lit in the
// throughput grade color, the rest are dim.
const (
	arcSpan = 13 // half-characters across the semicircle (0..arcSpan)
	redFrac = 0.8
)

// RenderTach lays out a half-dial: dim base ticks, a lit needle, and a redline
// zone on the far right, plus a numeric readout.
func RenderTach(s Snapshot) string {
	frac := clampF(s.TPS/s.Redline, 0, 1)
	idx, _ := grade(frac)
	needle := int(frac * float64(arcSpan)) // 0..arcSpan
	redSpan := float64(arcSpan) * redFrac  // var, not const, so int() truncates
	redTick := int(redSpan)
	var ticks strings.Builder
	for i := 0; i <= arcSpan; i++ {
		var mark string
		if i > redTick {
			mark = "┃" // redline ticks are full-height bars
		} else {
			mark = "│"
		}
		switch {
		case i == needle && s.TPS > 0:
			ticks.WriteString(c256(231, "◆")) // white needle
		case i < needle:
			ticks.WriteString(c256(idx, mark))
		default:
			ticks.WriteString(c256(240, mark)) // dim unlit ticks
		}
	}
	return fmt.Sprintf("%s %s%4.0ft/s%s", ticks.String(), dim, s.TPS, reset)
}

// ── gauge 3: sparkline ─────────────────────────────────────────────────────

var sparkRunes = []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

// RenderSparkline draws a rolling waveform of the last few TPS samples. The
// newest sample is highlighted; the rest shade toward dim, so a revving
// engine shows a rising ridge that brightens at the right edge.
func RenderSparkline(s Snapshot) string {
	const w = 16
	n := len(s.History)
	if n == 0 {
		return strings.Repeat("·", w) + fmt.Sprintf(" %s%4.0ft/s%s", dim, s.TPS, reset)
	}
	// normalize against the redline so the curve's vertical range tracks the
	// engine rather than the absolute peak (a slow stream would otherwise sit
	// at the top of the chart forever).
	scale := s.Redline
	if scale <= 0 {
		scale = floorRedline
	}
	var b strings.Builder
	start := 0
	if n > w {
		start = n - w
	}
	for i := start; i < n; i++ {
		v := clampF(s.History[i]/scale, 0, 1)
		glyph := sparkRunes[int(clampF(v*7, 0, 7))]
		age := n - 1 - i // 0 = newest
		switch {
		case age == 0:
			b.WriteString(c256(231, glyph)) // white head — the live sample
		case age < 3:
			b.WriteString(c256(83, glyph)) // bright green, fresh
		case age < 7:
			b.WriteString(c256(65, glyph)) // medium green
		default:
			b.WriteString(c256(240, glyph)) // dim tail
		}
	}
	// pad left if history is short so the readout stays right-aligned
	for i := n - start; i < w; i++ {
		b.WriteString("·")
	}
	return fmt.Sprintf("%s %s%4.0ft/s%s", b.String(), dim, s.TPS, reset)
}

// ── gauge 4: shift lights ──────────────────────────────────────────────────

const lightCount = 8

// RenderShiftLights draws an F1-style LED strip. Each LED maps to a fraction
// of the redline; LEDs below the current load stay off, the lit ones progress
// green → yellow → red, and the top LEDs blink when the engine is redlining.
// A "REDLINE" tag flashes once the needle enters the red zone.
func RenderShiftLights(s Snapshot) string {
	frac := clampF(s.TPS/s.Redline, 0, 1)
	lit := int(frac * float64(lightCount)) // how many LEDs are on
	redlining := frac >= redFrac
	blinkOn := s.Frame%2 == 0 // blink on alternate frames

	var b strings.Builder
	for i := 0; i < lightCount; i++ {
		var idx int
		on := i < lit
		// the highest LEDs are red regardless of grade; middle yellow; low green
		switch {
		case i >= lightCount-2:
			idx = 196 // bright red
		case i >= lightCount-4:
			idx = 220 // gold
		default:
			idx = 46 // bright green
		}
		if on {
			// redline LEDs blink off every other frame to scream "shift!"
			if i >= lightCount-2 && redlining && !blinkOn {
				b.WriteString(c256(88, "●")) // dim red when blinking off
			} else {
				b.WriteString(c256(idx, "●"))
			}
		} else {
			b.WriteString(c256(238, "○")) // dim gray unlit LED (visible on dark+light)
		}
		b.WriteString(" ")
	}
	tag := "      "
	if redlining {
		if blinkOn {
			tag = c256(196, " RED! ")
		} else {
			tag = c256(88, " red! ")
		}
	}
	return fmt.Sprintf("%s%s %s%4.0ft/s%s", strings.TrimRight(b.String(), " "), tag, dim, s.TPS, reset)
}

// reset / dim are shared ANSI escapes used by every gauge.
const (
	reset = "\x1b[0m"
	dim   = "\x1b[38;5;240m"
)
