// Package tps measures streaming tokens-per-second and renders it as RPM-style
// gauges for the status line. Live TPS is estimated from streaming deltas
// (roughly 4 chars per token) because providers only report exact token counts
// once a turn finishes; the estimate is good enough to feel the throttle.
package tps

import (
	"strings"
	"sync"
	"time"
)

// charsPerToken is the rough heuristic mapping streamed characters to tokens.
// It under-counts whitespace-only deltas (a delta is never zero tokens once it
// arrives), so Add floors every event at one token.
const charsPerToken = 4

// floorRedline is the gauge's resting maximum. Until sustained throughput
// proves otherwise, the dial pegs "fast" at this many tokens/sec — so even a
// modest stream feels like it's revving. As peak rises, the redline grows to
// keep headroom (see redlineFor), exactly like a tachometer whose scale tracks
// the engine it's bolted to. 90 t/s is a realistic "fast model" top speed.
const floorRedline = 90.0

// window is the sliding interval TPS is averaged over. Short enough to feel
// responsive, long enough to smooth per-delta jitter (a burst of tiny deltas
// shouldn't spike the needle to infinity).
const window = time.Second

// sampleCap bounds the rolling history slice that sparkline + shift-light
// gauges render from. ~1.5s of 50ms frames is enough curve to read a trend.
const sampleCap = 32

// Tracker timestamps streaming-token arrivals and derives instantaneous TPS
// over a sliding window. It is safe for concurrent Add, Sample, and Snapshot
// calls so a producer and renderer can run independently.
type Tracker struct {
	mu      sync.Mutex
	now     func() time.Time
	events  []event // ring of recent arrivals, oldest first
	peak    float64
	samples []float64 // rolling TPS snapshots, oldest first
	frame   int       // ticked by Sample; gauges use it for blink/jitter
	// est maps a delta string to an estimated token count. Default:
	// len(trimmed)/4 floored to 1. Override for tests/demos that drive the
	// tracker with known token counts instead of text.
	est func(string) int
}

type event struct {
	t      time.Time
	tokens int
}

// Option configures a Tracker.
type Option func(*Tracker)

// WithNow replaces the clock (test/demo seam).
func WithNow(now func() time.Time) Option {
	return func(t *Tracker) { t.now = now }
}

// WithEstimator replaces the delta→tokens estimator. The default floors every
// arrival at one token so an empty/whitespace delta still counts.
func WithEstimator(f func(string) int) Option {
	return func(t *Tracker) { t.est = f }
}

// New returns a ready Tracker.
func New(opts ...Option) *Tracker {
	t := &Tracker{
		now: time.Now,
		est: estTokens,
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// estTokens is the default streaming-delta→tokens heuristic: ~4 chars/token,
// never zero (an arriving delta is at least one token).
func estTokens(s string) int {
	n := len(strings.TrimSpace(s)) / charsPerToken
	if n < 1 {
		return 1
	}
	return n
}

// Add feeds one streamed delta into the tracker.
func (t *Tracker) Add(delta string) { t.AddTokens(t.est(delta)) }

// AddTokens feeds an explicit token count (used by tests/demos and by the
// real-usage path that could correct the estimate mid-turn).
func (t *Tracker) AddTokens(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pushLocked(event{t: t.now(), tokens: n})
}

// pushLocked records an arrival and trims events outside the window. Peak is
// NOT updated here: instantaneous TPS over a tiny span (the first few deltas
// of a stream) spikes artificially, which would inflate the redline. Peak
// tracks the frame-sampled signal instead (see Sample), which is stable.
func (t *Tracker) pushLocked(e event) {
	t.events = append(t.events, e)
	cutoff := e.t.Add(-window)
	// drop expired events from the front (arrivals are monotonic)
	for len(t.events) > 0 && t.events[0].t.Before(cutoff) {
		t.events = t.events[1:]
	}
}

// tpsLocked computes instantaneous tokens/sec over the window ending at now.
// Only arrivals within [now-window, now] count, so a burst that ended long ago
// decays to zero as it scrolls out of the window — the gauge winds down instead
// of freezing at the burst rate.
func (t *Tracker) tpsLocked(now time.Time) float64 {
	cutoff := now.Add(-window)
	var tokens int
	var oldest time.Time
	for _, e := range t.events {
		if e.t.Before(cutoff) {
			continue
		}
		if oldest.IsZero() || e.t.Before(oldest) {
			oldest = e.t
		}
		tokens += e.tokens
	}
	if tokens == 0 {
		return 0
	}
	span := now.Sub(oldest)
	if span <= 0 {
		return float64(tokens) // simultaneous burst: report the raw count
	}
	// Floor the divisor at minSpan so the gauge needs a sliver of real elapsed
	// time before it reports a rate. Without this, a Sample() taken microseconds
	// after the first Add() divides one token by a few µs and spikes to hundreds
	// of thousands of t/s. minSpan is short enough that the needle still sweeps
	// up within ~200ms — it just ramps instead of teleporting.
	if span < minSpan {
		span = minSpan
	}
	return float64(tokens) / span.Seconds()
}

// minSpan is the minimum elapsed time tpsLocked will divide by, so a fresh
// stream ramps its reading instead of spiking from a single near-instant
// arrival. 200ms feels responsive yet avoids sub-millisecond blowups.
const minSpan = 200 * time.Millisecond

// TPS returns the current instantaneous tokens/sec.
func (t *Tracker) TPS() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tpsLocked(t.now())
}

// Sample snapshots the current TPS into the rolling history that sparkline and
// shift-light gauges render from. Call it on a fixed frame tick (the TUI's
// spinner tick or a dedicated 50ms frame). A zero TPS is still recorded so a
// stopped stream visibly decays back to the baseline rather than freezing.
func (t *Tracker) Sample() {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.tpsLocked(t.now())
	t.frame++
	if v > t.peak {
		t.peak = v // sampled signal is stable; first-delta spikes don't leak in
	}
	t.samples = append(t.samples, v)
	if len(t.samples) > sampleCap {
		t.samples = t.samples[len(t.samples)-sampleCap:]
	}
}

// Snapshot is an immutable view of the tracker for renderers. Copying the
// history out keeps gauges from racing the writer.
type Snapshot struct {
	TPS     float64   // current instantaneous tokens/sec
	Peak    float64   // highest TPS observed since the last Reset
	History []float64 // rolling samples, oldest first (len <= sampleCap)
	Redline float64   // gauge maximum; grows with peak to keep headroom
	Frame   int       // monotonically increasing tick; gauges use it for blink/jitter
}

// Snapshot returns a copy of the current state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	hist := make([]float64, len(t.samples))
	copy(hist, t.samples)
	cur := t.tpsLocked(t.now())
	return Snapshot{
		TPS:     cur,
		Peak:    t.peak,
		History: hist,
		Redline: redlineFor(t.peak),
		Frame:   t.frame,
	}
}

// redlineFor scales the gauge maximum to the engine: peg the resting dial at
// floorRedline, then climb with the peak so a fast stream still shows headroom
// (needle near, but not glued to, the top).
func redlineFor(peak float64) float64 {
	if peak*1.1 > floorRedline {
		return peak * 1.1
	}
	return floorRedline
}

// Reset zeroes all counters — call when a turn ends so the gauge winds down
// from a clean baseline on the next one.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = nil
	t.peak = 0
	t.samples = nil
	t.frame = 0
}
