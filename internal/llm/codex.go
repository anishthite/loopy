package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/context-labs/loopy/internal/codexauth"
)

// Codex talks to the ChatGPT Codex Responses endpoint using locally managed
// OAuth credentials.
type Codex struct {
	BaseURL string
	Source  *codexauth.Source
	HTTP    *http.Client
}

var _ Client = (*OpenAI)(nil)
var _ Client = (*Codex)(nil)

func NewCodex(baseURL string, source *codexauth.Source) *Codex {
	return &Codex{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Source:  source,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

// Models deliberately skips the Codex catalog. The subscription endpoint does
// not expose the public /models contract, so configured limits remain the
// source of truth.
func (*Codex) Models(context.Context) ([]ModelInfo, error) { return []ModelInfo{}, nil }

// Stream maps the current conversation to a Responses API request and folds
// its SSE events back into the existing loopy message shape.
func (c *Codex) Stream(ctx context.Context, req Request, onText, onThink func(string)) (Message, Usage, error) {
	body, err := json.Marshal(codexRequest(req, true))
	if err != nil {
		return Message{}, Usage{}, err
	}
	resp, err := c.post(ctx, body, true)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Message{}, Usage{}, httpError(resp)
	}

	msg := Message{Role: "assistant"}
	var usage Usage
	calls := callCollector{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event responseEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.Type == "error" || event.Type == "response.failed" {
			if event.Error.Message != "" {
				return Message{}, usage, fmt.Errorf("api error: %s", event.Error.Message)
			}
			return Message{}, usage, errors.New("codex response failed")
		}
		switch event.Type {
		case "response.output_text.delta":
			msg.Content += event.Delta
			if onText != nil && event.Delta != "" {
				onText(event.Delta)
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.reasoning.delta":
			if onThink != nil && event.Delta != "" {
				onThink(event.Delta)
			}
		case "response.output_item.added":
			calls.add(event.Item, false)
		case "response.output_item.done", "response.function_call_arguments.done":
			calls.add(event.Item, true)
			if event.CallID != "" {
				calls.add(responseItem{CallID: event.CallID, Arguments: event.Arguments}, true)
			}
		case "response.function_call_arguments.delta":
			calls.delta(event.CallID, event.Delta)
		case "response.completed":
			usage = event.Response.Usage.usage()
			calls.addAll(event.Response.Output)
		}
	}
	if err := scanner.Err(); err != nil {
		return Message{}, usage, err
	}
	msg.ToolCalls = calls.calls
	return msg, usage, nil
}

// Complete makes the non-streaming Responses request used by compaction and
// goal formulation.
func (c *Codex) Complete(ctx context.Context, req Request) (string, Usage, error) {
	body, err := json.Marshal(codexRequest(req, false))
	if err != nil {
		return "", Usage{}, err
	}
	resp, err := c.post(ctx, body, false)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", Usage{}, httpError(resp)
	}
	var out response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&out); err != nil {
		return "", Usage{}, err
	}
	var text strings.Builder
	for _, item := range out.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" || content.Type == "text" {
				text.WriteString(content.Text)
			}
		}
	}
	if text.Len() == 0 {
		return "", out.Usage.usage(), errors.New("no text in Codex completion response")
	}
	return text.String(), out.Usage.usage(), nil
}

func (c *Codex) post(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	if c.Source == nil {
		return nil, codexauth.ErrLoginRequired
	}
	creds, err := c.Source.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/codex/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	hr.Header.Set("ChatGPT-Account-ID", creds.AccountID)
	hr.Header.Set("Originator", "loopy")
	hr.Header.Set("OpenAI-Beta", "responses=experimental")
	hr.Header.Set("Content-Type", "application/json")
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	return c.httpClient().Do(hr)
}

func (c *Codex) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &HTTPError{Status: resp.Status, Body: strings.TrimSpace(string(body))}
}

type codexRequestBody struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []any          `json:"input"`
	Tools           []responseTool `json:"tools,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
	Store             bool   `json:"store"`
	Stream            bool   `json:"stream"`
	ToolChoice        string `json:"tool_choice"`
	ParallelToolCalls bool   `json:"parallel_tool_calls"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func codexRequest(req Request, stream bool) codexRequestBody {
	req.Messages = repairToolHistory(stripAuthored(req.Messages))
	body := codexRequestBody{
		Model:             req.Model,
		Input:             []any{},
		Store:             false,
		Stream:            stream,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		MaxOutputTokens:   req.MaxTokens,
	}
	var instructions []string
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			instructions = append(instructions, msg.TextContent())
			continue
		}
		switch msg.Role {
		case "user", "assistant":
			content := []responseContent{}
			if text := msg.TextContent(); text != "" {
				kind := "input_text"
				if msg.Role == "assistant" {
					kind = "output_text"
				}
				content = append(content, responseContent{Type: kind, Text: text})
			}
			for _, part := range msg.Parts {
				if part.Type == "image_url" && part.ImageURL != nil {
					content = append(content, responseContent{Type: "input_image", ImageURL: part.ImageURL.URL})
				}
			}
			if len(content) > 0 {
				body.Input = append(body.Input, responseMessage{Type: "message", Role: msg.Role, Content: content})
			}
			for _, call := range msg.ToolCalls {
				body.Input = append(body.Input, responseItem{
					Type:      "function_call",
					CallID:    call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				})
			}
		case "tool":
			body.Input = append(body.Input, responseToolOutput{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: msg.Content,
			})
		}
	}
	body.Instructions = strings.Join(instructions, "\n\n")
	if req.ReasoningEffort != "" {
		body.Reasoning = &struct {
			Effort string `json:"effort"`
		}{Effort: req.ReasoningEffort}
	}
	for _, tool := range req.Tools {
		body.Tools = append(body.Tools, responseTool{
			Type:        tool.Type,
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return body
}

type responseMessage struct {
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Content []responseContent `json:"content"`
}

type responseContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responseToolOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type responseEvent struct {
	Type      string       `json:"type"`
	Delta     string       `json:"delta"`
	CallID    string       `json:"call_id"`
	Arguments string       `json:"arguments"`
	Item      responseItem `json:"item"`
	Response  response     `json:"response"`
	Error     struct {
		Message string `json:"message"`
	} `json:"error"`
}

type response struct {
	Output []responseItem `json:"output"`
	Usage  responseUsage  `json:"usage"`
}

type responseItem struct {
	Type      string            `json:"type"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Content   []responseContent `json:"content"`
}

type responseUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

func (u responseUsage) usage() Usage {
	usage := Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: u.InputTokensDetails.CachedTokens}
	}
	return usage
}

type callCollector struct {
	calls   []ToolCall
	byID    map[string]int
	unnamed int
}

func (c *callCollector) add(item responseItem, replace bool) {
	if item.Type != "" && item.Type != "function_call" {
		return
	}
	if item.CallID == "" && item.Name == "" && item.Arguments == "" {
		return
	}
	if c.byID == nil {
		c.byID = map[string]int{}
	}
	id := item.CallID
	if id == "" {
		id = fmt.Sprintf("output-%d", c.unnamed)
		c.unnamed++
	}
	index, ok := c.byID[id]
	if !ok {
		index = len(c.calls)
		c.byID[id] = index
		c.calls = append(c.calls, ToolCall{ID: id, Type: "function"})
	}
	call := &c.calls[index]
	if item.Name != "" {
		call.Function.Name = item.Name
	}
	if item.Arguments != "" {
		if replace {
			call.Function.Arguments = item.Arguments
		} else {
			call.Function.Arguments += item.Arguments
		}
	}
}

func (c *callCollector) delta(callID, delta string) {
	if callID == "" || delta == "" {
		return
	}
	c.add(responseItem{CallID: callID, Arguments: delta}, false)
}

func (c *callCollector) addAll(items []responseItem) {
	for _, item := range items {
		c.add(item, true)
	}
}
