// Package tps gauge renderers. Each Render takes a Snapshot and returns a
// fixed-width ANSI-styled string suitable for a one-line status bar. Colors
// are 256-color escape codes (not lipgloss) so the renderers work standalone
// in cmd/tps-demo and can be re-wrapped by the TUI.
package tps

import (
	"fmt"
	"strings"
)

// ramp is a smooth green→yellow→red gradient through the xterm-256 color cube
// (46 bright green … 226 yellow … 196 red). gradAt maps a load fraction [0,1]
// to a color along it, so a gauge's lit band shades green at the low end to
// red at the top — the same mood as a 3-step grade, but continuous, the way a
// real tach's colored arc reads. frac is a fraction of the redline, so the
// gradient's mood tracks the engine, not absolute token counts.
var ramp = []int{46, 82, 118, 154, 190, 226, 214, 208, 202, 196}

func gradAt(frac float64) int {
	last := float64(len(ramp) - 1)
	return ramp[int(clampF(frac*last, 0, last))]
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

// RenderBar draws a horizontal fill bar shaded green→yellow→red along its
// length, over a dim ░ track so the bar's frame is always visible (at low load
// you see a short green fill in a long empty rail, not a nub in the void). A
// trailing "❯" marks the leading edge and a numeric TPS readout rides the right.
func RenderBar(s Snapshot) string {
	frac := clampF(s.TPS/s.Redline, 0, 1)
	total := float64(barWidth) * frac
	full := int(total)
	part := int((total - float64(full)) * 8) // 0..7
	var bb strings.Builder
	for i := 0; i < barWidth; i++ {
		switch {
		case i < full:
			bb.WriteString(c256(gradAt(float64(i)/barWidth), "█"))
		case i == full && part > 0:
			bb.WriteString(c256(gradAt(float64(full)/barWidth), barRunes[part]))
		default:
			bb.WriteString(c256(238, "░")) // dim track: the bar's frame always shows
		}
	}
	lead := c256(gradAt(frac), "❯")
	return fmt.Sprintf("%s%s %s%4.0ft/s%s", bb.String(), lead, dim, s.TPS, reset)
}

// ── gauge 2: tach ──────────────────────────────────────────────────────────

// RenderTach draws an analog tachometer: a 180° arc of tick marks with a
// sweeping needle. The last 20% of the arc is the redline zone. The needle
// sits at frac of the sweep; tick marks below the needle light up along a
// green→red gradient (a real tach's colored arc), the redline ticks stay
// dim-red even unlit so the danger band always reads, the rest are dim.
const (
	arcSpan = 13 // half-characters across the semicircle (0..arcSpan)
	redFrac = 0.8
)

// RenderTach lays out a half-dial: dim base ticks, a gradient lit band up to a
// white needle, and a dim-red redline zone on the far right, plus a readout.
func RenderTach(s Snapshot) string {
	frac := clampF(s.TPS/s.Redline, 0, 1)
	needle := int(frac * float64(arcSpan)) // 0..arcSpan
	redSpan := float64(arcSpan) * redFrac  // var, not const, so int() truncates
	redTick := int(redSpan)
	var ticks strings.Builder
	for i := 0; i <= arcSpan; i++ {
		pos := float64(i) / float64(arcSpan)
		mark := "│"
		if i > redTick {
			mark = "┃" // redline ticks are full-height bars
		}
		switch {
		case i == needle && s.TPS > 0:
			ticks.WriteString(c256(231, "◆")) // white needle
		case i < needle:
			ticks.WriteString(c256(gradAt(pos), mark)) // gradient lit band
		case i > redTick:
			ticks.WriteString(c256(88, mark)) // dim-red danger band shows even at idle
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

// ── gauge 5: tapered tach bars ──────────────────────────────────────────────
//
// These horizontal bars are for the demo lab. The load fills left→right, with
// a denser leading edge that tapers to slender trailing segments. The second
// row adds thickness or a marker. The single-row gauges above still own the
// real status-line slot.

const (
	tachRows              = 2
	tachSegments          = 14
	tachLeadingSegmentRun = 3
)

// tachReadout is the numeric t/s label pinned to a meter's bottom row.
func tachReadout(s Snapshot) string {
	return fmt.Sprintf(" %s%4.0ft/s%s", dim, s.TPS, reset)
}

func tachFilled(s Snapshot) int {
	frac := clampF(s.TPS/s.Redline, 0, 1)
	filled := int(frac * float64(tachSegments))
	if frac > 0 && filled == 0 {
		return 1
	}
	return filled
}

func tachPeakFilled(s Snapshot) int {
	frac := clampF(s.Peak/s.Redline, 0, 1)
	filled := int(frac * float64(tachSegments))
	if frac > 0 && filled == 0 {
		return 1
	}
	return filled
}

func tachSegmentFrac(i int) float64 {
	return float64(i) / float64(tachSegments-1)
}

func tachLeadingGlyph(segment, activeLevel int, glyph string) string {
	if segment >= activeLevel-tachLeadingSegmentRun+1 && segment < activeLevel {
		return "▓"
	}
	return glyph
}

func tachCell(idx int, glyph string) string {
	return c256(idx, glyph)
}

func tachLine(filled int, litGlyph string, markerAt, markerIdx int, markerGlyph string) string {
	activeLevel := filled - 1
	parts := make([]string, tachSegments)
	for i := range parts {
		switch {
		case i == markerAt:
			parts[i] = tachCell(markerIdx, markerGlyph)
		case i < filled:
			parts[i] = tachCell(gradAt(tachSegmentFrac(i)), tachLeadingGlyph(i, activeLevel, litGlyph))
		default:
			parts[i] = tachCell(238, "░")
		}
	}
	return strings.Join(parts, " ")
}

func tachMarkerLine(markerAt, markerIdx int, markerGlyph string) string {
	parts := make([]string, tachSegments)
	for i := range parts {
		if i == markerAt {
			parts[i] = tachCell(markerIdx, markerGlyph)
		} else {
			parts[i] = " "
		}
	}
	return strings.Join(parts, " ")
}

// RenderTachBarCap draws a tapered segmented fill; the active segment is white.
func RenderTachBarCap(s Snapshot) string {
	filled := tachFilled(s)
	level := filled - 1
	line := tachLine(filled, "▌", level, 231, "█")
	return line + "\n" + line + tachReadout(s)
}

// RenderTachBarNeedle draws a muted segmented fill with a ▲ marker below it.
func RenderTachBarNeedle(s Snapshot) string {
	filled := tachFilled(s)
	level := filled - 1
	return tachLine(filled, "▌", level, 231, "█") + "\n" +
		tachMarkerLine(level, 231, "▲") + tachReadout(s)
}

// RenderTachBarPeak draws the current fill plus a dim-red peak-hold mark.
func RenderTachBarPeak(s Snapshot) string {
	filled := tachFilled(s)
	level := filled - 1
	peak := tachPeakFilled(s) - 1
	return tachLine(filled, "▌", level, 231, "█") + "\n" +
		tachMarkerLine(peak, 88, "▔") + tachReadout(s)
}

// RenderTachBarBlink pulses the active segment on alternate frames.
func RenderTachBarBlink(s Snapshot) string {
	filled := tachFilled(s)
	level := filled - 1
	idx, glyph := 231, "█"
	if s.Frame%2 != 0 {
		idx, glyph = 196, "▒"
	}
	line := tachLine(filled, "▌", level, idx, glyph)
	return line + "\n" + line + tachReadout(s)
}

// reset / dim are shared ANSI escapes used by every gauge.
const (
	reset = "\x1b[0m"
	dim   = "\x1b[38;5;240m"
)
