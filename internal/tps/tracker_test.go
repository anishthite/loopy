package tps

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fakeClock is a controllable time source for deterministic TPS math.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestTrackerTPS(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))

	// 50 tokens in 1s == 50 t/s.
	for i := 0; i < 50; i++ {
		tr.Add("x")
		clock.t = clock.t.Add(20 * time.Millisecond)
	}
	if got := tr.TPS(); got < 45 || got > 55 {
		t.Fatalf("TPS after 50 tok/s stream = %.1f, want ~50", got)
	}
}

func TestTrackerDecaysAfterWindow(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))

	// burst 100 tokens instantly.
	for i := 0; i < 100; i++ {
		tr.Add("x")
	}
	if got := tr.TPS(); got < 95 {
		t.Fatalf("TPS right after burst = %.1f, want ~100", got)
	}
	// advance well past the window; the events should expire and TPS -> 0.
	clock.t = clock.t.Add(2 * time.Second)
	if got := tr.TPS(); got != 0 {
		t.Fatalf("TPS after window expired = %.1f, want 0", got)
	}
}

func TestTrackerPeakPersists(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))

	for i := 0; i < 200; i++ {
		tr.Add("x")
		tr.Sample()                                 // peak tracks the frame-sampled signal, not per-push spikes
		clock.t = clock.t.Add(5 * time.Millisecond) // ~200 tok/s briefly
	}
	clock.t = clock.t.Add(2 * time.Second)
	snap := tr.Snapshot()
	if snap.Peak < 150 {
		t.Fatalf("peak = %.1f, want >=150", snap.Peak)
	}
	if snap.TPS != 0 {
		t.Fatalf("current TPS should be 0 after decay, got %.1f", snap.TPS)
	}
}

func TestRedlineScalesWithPeak(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))

	// idle: redline is the floor.
	if got := tr.Snapshot().Redline; got != floorRedline {
		t.Fatalf("idle redline = %.0f, want %.0f", got, floorRedline)
	}
	// rev past the floor: redline climbs to 1.1x peak.
	for i := 0; i < 200; i++ {
		tr.Add("x")
		tr.Sample()
		clock.t = clock.t.Add(5 * time.Millisecond)
	}
	snap := tr.Snapshot()
	want := snap.Peak * 1.1
	if snap.Redline < want-1 || snap.Redline > want+1 {
		t.Fatalf("redline = %.1f, want ~%.1f (1.1x peak)", snap.Redline, want)
	}
}

func TestSampleHistory(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))

	for i := 0; i < 40; i++ { // exceeds sampleCap
		tr.Add("x")
		tr.Sample()
		clock.t = clock.t.Add(50 * time.Millisecond)
	}
	snap := tr.Snapshot()
	if len(snap.History) > sampleCap {
		t.Fatalf("history len = %d, want <= %d", len(snap.History), sampleCap)
	}
	if len(snap.History) != sampleCap {
		t.Fatalf("history len = %d, want exactly %d after overflow", len(snap.History), sampleCap)
	}
}

func TestResetClears(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))
	for i := 0; i < 50; i++ {
		tr.Add("x")
	}
	tr.Sample()
	tr.Sample()
	tr.Reset()
	snap := tr.Snapshot()
	if snap.TPS != 0 || snap.Peak != 0 || len(snap.History) != 0 {
		t.Fatalf("after Reset: %+v, want all zero", snap)
	}
}

func TestDefaultEstimatorFloorsEmpty(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now))
	// a whitespace-only delta is still >=1 token.
	before := len(tr.events)
	tr.Add("   ")
	if len(tr.events) != before+1 {
		t.Fatal("empty delta should still record one token")
	}
	if tr.events[0].tokens != 1 {
		t.Fatalf("empty delta tokens = %d, want 1", tr.events[0].tokens)
	}
}

func TestRenderersNoPanicAndContainTPS(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 5 }))
	for i := 0; i < 100; i++ {
		tr.Add("x")
		tr.Sample()
		clock.t = clock.t.Add(10 * time.Millisecond)
	}
	snap := tr.Snapshot()
	for name, got := range map[string]string{
		"bar":           RenderBar(snap),
		"tach":          RenderTach(snap),
		"sparkline":     RenderSparkline(snap),
		"shiftlights":   RenderShiftLights(snap),
		"tachbarcap":    RenderTachBarCap(snap),
		"tachbarneedle": RenderTachBarNeedle(snap),
		"tachbarpeak":   RenderTachBarPeak(snap),
		"tachbarblink":  RenderTachBarBlink(snap),
	} {
		if got == "" {
			t.Errorf("%s: empty render", name)
		}
		if !strings.Contains(got, "t/s") {
			t.Errorf("%s: render missing t/s readout: %q", name, got)
		}
		// tach bars must produce tachRows lines.
		if strings.HasPrefix(name, "tachbar") && strings.Count(got, "\n")+1 != tachRows {
			t.Errorf("%s: %d lines, want %d", name, strings.Count(got, "\n")+1, tachRows)
		}
	}
}

// TestTachBarIdleAndFull pins the level-marker logic at the two extremes so a
// future change to the segment math can't silently break the cap/needle markers.
func TestTachBarIdleAndFull(t *testing.T) {
	clock := &fakeClock{t: time.UnixMilli(0)}
	tr := New(WithNow(clock.now), WithEstimator(func(string) int { return 1 }))
	idle := tr.Snapshot() // TPS 0 → no lit rows, no marker
	if strings.Contains(RenderTachBarCap(idle), "\x1b[38;5;231m") {
		t.Error("idle cap meter should have no white level cap")
	}
	// peg the redline: 300 tokens/sec for the whole window.
	for i := 0; i < 300; i++ {
		tr.Add("x")
		clock.t = clock.t.Add(time.Millisecond * 1000 / 300)
	}
	full := tr.Snapshot()
	if !strings.Contains(RenderTachBarCap(full), "\x1b[38;5;231m") {
		t.Error("full cap meter should show the white level cap")
	}
	if !strings.Contains(RenderTachBarNeedle(full), "▲") {
		t.Error("full needle meter should show the ▲ level marker")
	}
}

func TestTachBarTapersAtLevel(t *testing.T) {
	snap := Snapshot{TPS: 50, Redline: 100}
	line, _, _ := strings.Cut(ansi.Strip(RenderTachBarCap(snap)), "\n")
	segments := strings.Split(line, " ")
	want := []string{"▌", "▌", "▌", "▌", "▓", "▓", "█", "░", "░", "░", "░", "░", "░", "░"}
	if got, want := len(segments), len(want); got != want {
		t.Fatalf("segment count = %d, want %d", got, want)
	}

	for i, segment := range segments {
		if segment != want[i] {
			t.Errorf("segment %d = %q, want %q", i, segment, want[i])
		}
	}
}
