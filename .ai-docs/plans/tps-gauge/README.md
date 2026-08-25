# Live TPS status gauge

Branch: `monterrey`

## What this does

Adds a small live tokens-per-second indicator to loopy's status line. It
estimates completion tokens from streamed text, smooths the signal over one
second, and renders a default tachometer-style gauge. `tpsGauge` can select a
bar, tach, sparkline, or shift-lights treatment; `off` hides it.

The demo is a self-contained simulated stream: `go run ./cmd/tps-demo -snap`
renders deterministic frames without a TTY, while `go run ./cmd/tps-demo`
runs the interactive visual lab.

## Goal

Make the current completion throughput immediately readable without changing
the agent loop or persisting live-only display state.

## Non-goals

- Exact provider token accounting while a completion is in flight.
- Persisting samples across turns or resumed sessions.
- A palette command for changing the gauge; the small configuration key is
  sufficient for this first version.

## Design

- `internal/tps/tracker.go`: bounded, mutex-protected rolling events and
  snapshots. Streaming callbacks send Bubble Tea messages, so the UI normally
  owns updates; the tracker remains safe for the standalone demo and tests.
- `internal/tps/gauges.go`: ANSI one-line status-gauge renderers. The thick
  tach bars remain demo-only because a status row must stay one line high.
- `internal/tui/tui.go`: feeds text deltas, samples the existing spinner tick,
  and resets the live-only tracker at turn boundaries.
- `internal/tui/status_test.go`: pins visibility, styles, and idle behavior.
- `cmd/tps-demo/main.go`: interactive simulated revs plus a deterministic
  `-snap` mode for easy inspection.
- `internal/config/config.go`, `docs/features.md`, and `README.md`: document
  the configuration and demo commands.

No goroutines or dependencies are added. The only existing goroutine involved
is the turn worker; its `prog.Send` messages are handled on the Bubble Tea UI
thread. The tracker lock makes explicit snapshots race-safe without changing
that lifecycle.

## Prior art

- `docs/learnings/other-harnesses/opencode/opencode-ux.md:88-96` describes a
  footer that puts live token/context/cost telemetry alongside a spinner.
- The existing status spend display in `internal/tui/tui.go:statusView` is the
  integration surface; `internal/tui/status_test.go` is its test home.

## Test plan

- Unit-test the rolling TPS estimate, decay, reset, redline, renderer output,
  and marker extremes in `internal/tps/tracker_test.go`.
- Unit-test status-gauge selection, hide-off/idle behavior, and reset in
  `internal/tui/status_test.go`.
- Run `go test -race ./internal/tps ./internal/tui ./cmd/tps-demo` and
  `task check`.
- Run `go run ./cmd/tps-demo -snap` as the headless demo acceptance check.

## Task breakdown

- [x] Inspect the existing TPS branch and preserve only TPS-related changes.
- [x] Apply the tracker, renderers, status integration, and demo.
- [x] Correct the demo controls and add focused status/config coverage.
- [x] Document the feature and demo command.
- [x] Run the test and demo gates; review the final diff.

## Validation

- Passed: `go test ./internal/config ./internal/tps ./internal/tui ./cmd/tps-demo`.
- Passed: `go test -race -v ./internal/tui ./cmd/tps-demo`.
- Passed: `go vet ./...` and `go run ./cmd/tps-demo -snap`; the interactive
  demo was launched and quit cleanly with `q`.
- `task check` is blocked before vet/test by the pre-existing, unchanged
  formatting of `internal/tui/compact_log.go`.
- `go test ./...` otherwise passes, but the three browser dedicated-mode E2E
  tests fail in this cloud image because it has no X server or `$DISPLAY`.
