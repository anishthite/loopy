package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/context-labs/loopy/internal/codexauth"
)

func TestCodexStreamRequestAndEvents(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/codex/responses" {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		for header, expected := range map[string]string{
			"Authorization":      "Bearer access",
			"ChatGPT-Account-ID": "account",
			"Originator":         "loopy",
			"OpenAI-Beta":        "responses=experimental",
			"Accept":             "text/event-stream",
			"Content-Type":       "application/json",
		} {
			if value := r.Header.Get(header); value != expected {
				http.Error(w, header+" = "+value, http.StatusBadRequest)
				return
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"bash\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"call_id\":\"call-1\",\"delta\":\"{\\\"command\\\":\\\"p\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"call_id\":\"call-1\",\"arguments\":\"{\\\"command\\\":\\\"pwd\\\"}\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":5}}}}\n\n")
	}))
	defer srv.Close()

	tool := NewTool("bash", "run a command", `{"type":"object"}`)
	var text, think strings.Builder
	msg, usage, err := NewCodex(srv.URL, codexSource(t)).Stream(context.Background(), Request{
		Model:           "gpt-5.4",
		MaxTokens:       128000,
		ReasoningEffort: "high",
		Messages: []Message{
			{Role: "system", Content: "system prompt"},
			{Role: "user", Content: "inspect the repository"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "old-call", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"README.md"}`}}}},
			{Role: "tool", ToolCallID: "old-call", Name: "read", Content: "file contents"},
		},
		Tools: []Tool{tool},
	}, func(delta string) { text.WriteString(delta) }, func(delta string) { think.WriteString(delta) })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "done" || text.String() != "done" || think.String() != "plan" {
		t.Fatalf("message streams: msg=%+v text=%q think=%q", msg, text.String(), think.String())
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call-1" || msg.ToolCalls[0].Function.Name != "bash" || msg.ToolCalls[0].Function.Arguments != `{"command":"pwd"}` {
		t.Fatalf("tool calls: %+v", msg.ToolCalls)
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 7 || usage.Cached() != 5 {
		t.Fatalf("usage: %+v", usage)
	}
	if got["model"] != "gpt-5.4" || got["instructions"] != "system prompt" || got["stream"] != true || got["store"] != false || got["tool_choice"] != "auto" || got["parallel_tool_calls"] != true {
		t.Fatalf("request = %#v", got)
	}
	if got["max_output_tokens"] != float64(128000) {
		t.Fatalf("max output tokens = %#v", got["max_output_tokens"])
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", got["reasoning"])
	}
	input, ok := got["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input = %#v", got["input"])
	}
	if input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("tool history input = %#v", input)
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["name"] != "bash" {
		t.Fatalf("tools = %#v", got["tools"])
	}
}

func TestCodexComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), `"stream":true`) {
			http.Error(w, "complete streamed", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"summary"}]}],"usage":{"input_tokens":9,"output_tokens":2}}`)
	}))
	defer srv.Close()

	text, usage, err := NewCodex(srv.URL, codexSource(t)).Complete(context.Background(), Request{
		Model:    "gpt-5.4",
		Messages: []Message{{Role: "system", Content: "summarize"}, {Role: "user", Content: "history"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "summary" || usage.PromptTokens != 9 || usage.CompletionTokens != 2 {
		t.Fatalf("complete = %q, %+v", text, usage)
	}
}

func TestCodexModelsSkipsCatalog(t *testing.T) {
	models, err := NewCodex("http://unused", codexSource(t)).Models(context.Background())
	if err != nil || len(models) != 0 {
		t.Fatalf("models = %+v, %v", models, err)
	}
}

func codexSource(t *testing.T) *codexauth.Source {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"tokens":{"access_token":"access","refresh_token":"refresh","account_id":"account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &codexauth.Source{HomeDir: home}
}
