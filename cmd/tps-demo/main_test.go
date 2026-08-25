package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSpaceTogglesFloor(t *testing.T) {
	m := initial()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(model)
	if !m.isFloored {
		t.Fatal("first space press should floor the throttle")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(model)
	if m.isFloored {
		t.Fatal("second space press should return to coasting")
	}
}

func TestFlooredStepEmitsTokens(t *testing.T) {
	m := initial()
	m.isFloored = true
	m = m.step(frame)

	if got := m.tracker.Snapshot().TPS; got <= 0 {
		t.Fatalf("floored step TPS = %.1f, want > 0", got)
	}
}
