package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/context-labs/loopy/internal/agent"
	"github.com/context-labs/loopy/internal/config"
	"github.com/context-labs/loopy/internal/llm"
	"github.com/context-labs/loopy/internal/tps"
)

// statusModel builds a model with an agent so statusView has data.
func statusModel() *model {
	m := newGrowModel()
	m.agent = &agent.Agent{}
	return m
}

// The status line always renders below the input with directory, model
// (effort), provider, and session token spend — regardless of scroll or state.
func TestStatusLineAlwaysShown(t *testing.T) {
	m := statusModel()
	m.modelName = "kimi-k3-fast"
	m.provName = "inference"
	m.agent.Effort = "high"
	m.agent.AddUsage(llm.Usage{PromptTokens: 45230, CompletionTokens: 3120})

	v := m.View()
	for _, want := range []string{"kimi-k3-fast (high)", "inference", "45.2k", "3.1k"} {
		if !strings.Contains(v, want) {
			t.Errorf("status line should show %q\n--- view tail ---\n%s", want, tailLines(v, 6))
		}
	}
	// the directory is present (compacted to its last segments) — assert
	// against the actual cwd via shortCWD, not a hardcoded checkout name:
	// tests run from internal/tui regardless of the repo folder's name.
	if dir := shortCWD(); !strings.Contains(v, dir) {
		t.Errorf("status line should show the working directory %q\n%s", dir, tailLines(v, 6))
	}
}

// With no usage yet the spend reads zero, and effort off drops the parens.
func TestStatusLineDefaults(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"

	v := m.View()
	if !strings.Contains(v, "0/0 tok") {
		t.Errorf("empty session should read 0/0 tok\n%s", tailLines(v, 6))
	}
	if strings.Contains(v, "m (") {
		t.Errorf("effort off should not add parens\n%s", tailLines(v, 6))
	}
	if !strings.Contains(v, "  m   p  ") && !strings.Contains(v, " m   p ") {
		t.Errorf("bare model and provider should appear\n%s", tailLines(v, 6))
	}
}

func TestStatusLineTPSGauge(t *testing.T) {
	tests := []struct {
		name  string
		style string
		want  string
	}{
		{name: "default tach", want: "◆"},
		{name: "bar", style: "bar", want: "❯"},
		{name: "sparkline", style: "spark", want: "t/s"},
		{name: "shift lights", style: "lights", want: "●"},
		{name: "off", style: "off", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := statusModel()
			m.busy = true
			m.cfg = &config.Config{TPSGauge: tt.style}
			m.tpsTracker = tps.New()
			m.tpsTracker.AddTokens(12)
			m.tpsTracker.Sample()

			got := m.statusView()
			if tt.want == "" {
				if strings.Contains(got, "t/s") {
					t.Fatalf("off gauge should be hidden: %q", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("%s gauge missing %q: %q", tt.name, tt.want, got)
			}
			if gotWidth := ansi.StringWidth(got); gotWidth > m.width {
				t.Errorf("%s gauge status width = %d, want <= %d: %q", tt.name, gotWidth, m.width, got)
			}
		})
	}

	m := statusModel()
	m.cfg = &config.Config{}
	m.tpsTracker = tps.New()
	m.tpsTracker.AddTokens(12)
	if got := m.statusView(); strings.Contains(got, "t/s") {
		t.Errorf("idle gauge should be hidden: %q", got)
	}
}

func TestTruncLinePreservesANSISequences(t *testing.T) {
	got := truncLine("\x1b[31mabcdef\x1b[0m", 4)
	if width := ansi.StringWidth(got); width != 4 {
		t.Errorf("ANSI-aware truncated width = %d, want 4: %q", width, got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("truncated ANSI string should reset styling: %q", got)
	}
}

// Cached tokens surface in the spend segment.
func TestStatusLineShowsCached(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	u := llm.Usage{PromptTokens: 10000, CompletionTokens: 500}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 4000}
	m.agent.AddUsage(u)

	if got := m.statusView(); !strings.Contains(got, "10.0k(4.0k)/500 tok") {
		t.Errorf("cached tokens should show in the spend: %q", got)
	}
}

// The status line is the last content row before the bottom padding, sitting
// below the input even when the esc/quit warnings or completion menu show.
func TestStatusLineBelowInputAndWarnings(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.escClr = true // draft-clear warning armed

	v := m.View()
	lines := strings.Split(strings.TrimRight(v, "\n"), "\n")
	var inputRow, statusRow int
	for i, l := range lines {
		if strings.Contains(l, "Ask loopy anything") {
			inputRow = i
		}
		if strings.Contains(l, "0/0 tok") {
			statusRow = i
		}
	}
	if statusRow <= inputRow {
		t.Fatalf("status line should sit below the input (input=%d status=%d)\n%s", inputRow, statusRow, v)
	}
}

// Exactly one blank line separates the status line from whatever is above it,
// and the status line is the final row (no blank line below).
func TestStatusLineSpacing(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"

	lines := strings.Split(m.View(), "\n")
	var statusRow = -1
	for i, l := range lines {
		if strings.Contains(l, "0/0 tok") {
			statusRow = i
		}
	}
	if statusRow < 1 {
		t.Fatalf("status line not found\n%s", m.View())
	}
	if lines[statusRow-1] != "" {
		t.Errorf("want one blank line above the status line, got %q", lines[statusRow-1])
	}
	// the status line is the last row, with nothing below it
	if statusRow != len(lines)-1 {
		t.Errorf("status line should be the last row (row %d of %d lines)", statusRow, len(lines)-1)
	}
}

// tailLines returns the last n lines of s, for failure output.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Cost appears in the spend segment when the provider's catalog advertises
// pricing for the current model, and is hidden otherwise.
func TestStatusLineShowsCost(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.agent.Model = "priced"
	m.catalogs = map[string]config.Catalog{
		"p": {Models: []config.ModelInfoLite{{ID: "priced", InPrice: 1e-6, OutPrice: 5e-6, CacheReadPrice: 1e-7}}},
	}
	u := llm.Usage{PromptTokens: 10000, CompletionTokens: 1000}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 8000}
	m.agent.AddUsage(u)

	// (10k-8k)*1e-6 + 8k*1e-7 + 1k*5e-6 = 0.0078
	if got := m.statusView(); !strings.Contains(got, "$0.0078") {
		t.Errorf("cost should show in the spend: %q", got)
	}
}

func TestStatusLineHidesCostWithoutPricing(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.agent.Model = "unpriced"
	m.catalogs = map[string]config.Catalog{
		"p": {Models: []config.ModelInfoLite{{ID: "unpriced"}}},
	}
	m.agent.AddUsage(llm.Usage{PromptTokens: 10000, CompletionTokens: 1000})

	if got := m.statusView(); strings.Contains(got, "$") {
		t.Errorf("unpriced model should hide cost: %q", got)
	}

	// no catalog for the provider at all (startup fetch still in flight)
	m.catalogs = nil
	if got := m.statusView(); strings.Contains(got, "$") {
		t.Errorf("missing catalog should hide cost: %q", got)
	}
}

func TestFmtCost(t *testing.T) {
	if got := fmtCost(0.0134); got != "$0.0134" {
		t.Errorf("sub-dollar: %q", got)
	}
	if got := fmtCost(12.345); got != "$12.35" {
		t.Errorf("over a dollar: %q", got)
	}
}

// The /models fetch → catalog → cost pipeline keeps per-variant pricing
// distinct (kimi-k3-fast bills higher than kimi-k3) with nothing hardcoded:
// rates come from the provider's response body alone.
func TestSessionCostUsesFetchedPricing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
			{"id":"kimi-k3","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003"}},
			{"id":"kimi-k3-fast","pricing":{"prompt":"0.0000045","completion":"0.0000225","input_cache_read":"0.00000045"}}
		]}`))
	}))
	defer srv.Close()

	infos, err := llm.New(srv.URL, "k").Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lites := make([]config.ModelInfoLite, len(infos))
	for i, mi := range infos {
		lites[i] = config.ModelInfoLite{ID: mi.ID}
		if mi.Pricing != nil {
			lites[i].InPrice, lites[i].OutPrice, lites[i].CacheReadPrice = mi.Pricing.Rates()
		}
	}
	m := statusModel()
	m.provName = "inference"
	m.catalogs = map[string]config.Catalog{"inference": {Models: lites}}
	u := llm.Usage{PromptTokens: 31100, CompletionTokens: 360}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 20700}
	m.agent.AddUsage(u)

	m.agent.Model = "kimi-k3-fast"
	fast, ok := m.sessionCost()
	if !ok {
		t.Fatal("fast variant should be priced")
	}
	m.agent.Model = "kimi-k3"
	std, ok := m.sessionCost()
	if !ok {
		t.Fatal("standard variant should be priced")
	}
	if fast <= std {
		t.Errorf("kimi-k3-fast cost %v should exceed kimi-k3 %v", fast, std)
	}
	// exact: (31100-20700)*4.5e-6 + 20700*4.5e-7 + 360*22.5e-6
	if want := 0.064215; fast != want {
		t.Errorf("kimi-k3-fast cost = %v, want %v", fast, want)
	}
}
