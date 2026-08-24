// Package tools implements the agent's built-in tools.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/context-labs/loopy/internal/llm"
	"github.com/context-labs/loopy/internal/tools/bashrun"
)

// Tool is a named executable tool with a JSON schema.
type Tool struct {
	Def llm.Tool
	Run func(ctx context.Context, args json.RawMessage) (string, error)
}

// InteractiveRunner runs an interactive bash command with PTY-backed live I/O.
// The TUI installs one so the agent's bash tool can hand interactive prompts
// (sudo, ssh, gpg) to the user. ctx caps the whole run; keys feeds keystrokes
// the user types; the returned string is fed back to the model as tool output.
// Implementations must be safe to call from a goroutine that is not the UI
// thread, and must not block forever when no input arrives.
type InteractiveRunner interface {
	Run(ctx context.Context, command string, timeout time.Duration, keys <-chan []byte) string
}

// InteractiveBash is the hook installed by the TUI; nil means the agent's bash
// tool runs interactive commands itself using the non-interactive fallback
// (which fast-fails sudo-style prompts instead of hanging).
var InteractiveBash InteractiveRunner

// All returns the built-in tool set.
func All() []Tool {
	return []Tool{bashTool(), readTool(), writeTool(), editTool()}
}

// Hashline returns the experimental hashline tool set (see hashline.go):
// hashline_read and hashline_edit stand in for read and edit, addressing
// lines by staleness-checked LINE#HASH anchors. Gated on
// experimental.hashlineEdit in config.
func Hashline() []Tool {
	return []Tool{bashTool(), hashlineReadTool(), writeTool(), hashlineEditTool()}
}

// Defs returns the llm.Tool definitions for a tool set.
func Defs(ts []Tool) []llm.Tool {
	defs := make([]llm.Tool, len(ts))
	for i, t := range ts {
		defs[i] = t.Def
	}
	return defs
}

// Suggester returns the closest known tool names for an unknown one —
// installed by the agent (which knows the live MCP tool set) so a stale or
// typo'd tool call nudges the model toward the right name instead of
// dead-ending the turn.
var Suggester func(name string) []string

// Execute runs the named tool. Errors are returned as strings so they can be
// fed back to the model rather than aborting the loop.
func Execute(ctx context.Context, ts []Tool, name string, args json.RawMessage) string {
	for _, t := range ts {
		if t.Def.Function.Name == name {
			out, err := t.Run(ctx, args)
			if err != nil {
				return "Error: " + err.Error()
			}
			if out == "" {
				out = "(no output)"
			}
			return out
		}
	}
	msg := fmt.Sprintf("Error: unknown tool %q", name)
	if Suggester != nil {
		if hints := Suggester(name); len(hints) > 0 {
			msg += " — did you mean " + strings.Join(hints, " or ") + "?"
		}
	}
	return msg
}

const maxOutput = 50_000 // bytes of tool output fed back to the model

// Truncate caps tool output at maxOutput with a marker; exported for the MCP
// bridge, which flattens remote results into the same budget.
func Truncate(s string) string {
	return truncate(s)
}

func truncate(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-maxOutput)
}

// TruncateTail caps tool output at maxOutput bytes, keeping the tail (the end
// is usually where the error is). Exported for the TUI's `!` shell escape,
// which formats output exactly like the bash tool.
func TruncateTail(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return fmt.Sprintf("[... first %d bytes truncated]\n", len(s)-maxOutput) + s[len(s)-maxOutput:]
}

func bashTool() Tool {
	return Tool{
		Def: llm.NewTool("bash",
			"Execute a bash command in the current working directory and return its combined stdout/stderr. Use for running programs, git, searching (grep/rg), listing files, etc.",
			`{"type":"object","properties":{"command":{"type":"string","description":"The bash command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (default 120)"},"interactive":{"type":"boolean","description":"Run in a PTY so sudo/ssh-style password prompts work. Loopy stays in control of the terminal and forwards your keystrokes; the command is killed after 15s of no input. Use only for commands that genuinely need a password."}},"required":["command"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command     string  `json:"command"`
				Timeout     float64 `json:"timeout"`
				Interactive bool    `json:"interactive"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.Timeout <= 0 {
				a.Timeout = 120
			}
			dur := time.Duration(a.Timeout * float64(time.Second))

			// Interactive mode hands the live terminal to the user only when the
			// TUI has wired a runner. Without it we run non-interactively, which
			// fails sudo-style prompts fast instead of hanging on loopy's tty.
			if a.Interactive && InteractiveBash != nil {
				keys := make(chan []byte, 16)
				out := InteractiveBash.Run(ctx, a.Command, dur, keys)
				return TruncateTail(out), nil
			}

			res := bashrun.Run(ctx, bashrun.Options{
				Command: a.Command,
				Timeout: dur,
			})

			s := TruncateTail(res.Output)
			if res.TimedOut {
				return s + "\n(command timed out)", nil
			}
			if res.Exit != "" {
				return fmt.Sprintf("%s\n(%s)", s, res.Exit), nil
			}
			if s == "" {
				return "(no output)", nil
			}
			return s, nil
		},
	}
}

func readTool() Tool {
	return Tool{
		Def: llm.NewTool("read",
			"Read a file and return its contents with line numbers.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"offset":{"type":"number","description":"1-based line to start from"},"limit":{"type":"number","description":"Max lines to return (default 2000)"}},"required":["path"]}`),
		Run: readRun(false),
	}
}

// readRun renders the selected line window with either classic line numbers
// ("N\tline") or hashline tags ("N#HASH:line") for the hashline_read variant.
func readRun(hashlines bool) func(ctx context.Context, args json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return "", err
		}
		lines := strings.Split(string(data), "\n")
		start := max(a.Offset-1, 0)
		if start >= len(lines) {
			return "", fmt.Errorf("offset %d past end of file (%d lines)", a.Offset, len(lines))
		}
		limit := a.Limit
		if limit <= 0 {
			limit = 2000
		}
		end := min(start+limit, len(lines))
		var b strings.Builder
		for i := start; i < end; i++ {
			if hashlines {
				fmt.Fprintf(&b, "%d#%s:%s\n", i+1, computeLineHash(i+1, lines[i]), lines[i])
			} else {
				fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
			}
		}
		return truncate(b.String()), nil
	}
}

func hashlineReadTool() Tool {
	return Tool{
		Def: llm.NewTool("hashline_read",
			"Read a file with hashline annotations. Each line is prefixed with LINE#HASH (e.g. `5#ZP:  const x = 1;`). Use these LINE#HASH references in hashline_edit to make precise, staleness-checked edits.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"offset":{"type":"number","description":"1-based line to start from"},"limit":{"type":"number","description":"Max lines to return (default 2000)"}},"required":["path"]}`),
		Run: readRun(true),
	}
}

func hashlineEditTool() Tool {
	return Tool{
		Def: llm.NewTool("hashline_edit",
			`Edit a file using hashline references obtained from hashline_read. Each edit targets lines by their LINE#HASH tag, which acts as a staleness check — if the file changed since the last read, hash mismatches are caught before any mutation and the error returns the fresh tags.

Operations:
  replace: Replace line at pos (or range pos..end) with new lines. Single-line replace requires "current": the exact current content of the line at pos.
  append:  Insert lines after pos (or at end of file if pos omitted)
  prepend: Insert lines before pos (or at start of file if pos omitted)

Multiple edits are applied atomically (all-or-nothing on validation), bottom-up so line numbers stay valid within the batch.`,
			`{"type":"object","properties":{`+
				`"path":{"type":"string","description":"Path to the file to edit"},`+
				`"edits":{"type":"array","description":"Edit operations referencing LINE#HASH tags from hashline_read. Validated before any mutation.","items":{"type":"object","properties":{`+
				`"op":{"type":"string","enum":["replace","append","prepend"],"description":"replace line(s) at pos (through end if given); append after pos (default EOF); prepend before pos (default BOF)"},`+
				`"pos":{"type":"string","description":"Line reference as \"LINE#HASH\" (e.g. \"5#ZP\"). Required for replace; optional for append/prepend."},`+
				`"end":{"type":"string","description":"End of range for multi-line replace, as \"LINE#HASH\"."},`+
				`"lines":{"type":"array","items":{"type":"string"},"description":"New lines to insert or replace with (one string per line, no trailing newlines)"},`+
				`"current":{"type":"string","description":"Single-line replace only: the exact current content of the line at pos. Must match or the edit is rejected before any mutation."}`+
				`},"required":["op","lines"]}},`+
				`"create_if_missing":{"type":"boolean","description":"If true and the file does not exist, create it from the append/prepend lines. Default false."}`+
				`},"required":["path","edits"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path  string `json:"path"`
				Edits []struct {
					Op      string   `json:"op"`
					Pos     string   `json:"pos"`
					End     string   `json:"end"`
					Lines   []string `json:"lines"`
					Current *string  `json:"current"`
				} `json:"edits"`
				CreateIfMissing bool `json:"create_if_missing"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}

			data, err := os.ReadFile(a.Path)
			if os.IsNotExist(err) {
				if !a.CreateIfMissing {
					return "", fmt.Errorf("file not found: %s — use create_if_missing to create it", a.Path)
				}
				var newLines []string
				for _, e := range a.Edits {
					if e.Op == "append" || e.Op == "prepend" {
						newLines = append(newLines, e.Lines...)
					}
				}
				if len(newLines) == 0 {
					return "", fmt.Errorf("cannot create file: no append/prepend lines provided")
				}
				content := strings.Join(newLines, "\n")
				if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(a.Path, []byte(content), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("Created %s (%d lines)", a.Path, len(newLines)), nil
			}
			if err != nil {
				return "", err
			}

			edits := make([]hashlineEdit, len(a.Edits))
			for i, e := range a.Edits {
				h := hashlineEdit{op: e.Op, lines: e.Lines}
				switch e.Op {
				case "replace":
					if e.Pos == "" {
						return "", fmt.Errorf("edit %d: replace requires a \"pos\" reference", i)
					}
					pos, err := parseTag(e.Pos)
					if err != nil {
						return "", err
					}
					h.pos = &pos
					if e.End != "" {
						end, err := parseTag(e.End)
						if err != nil {
							return "", err
						}
						h.end = &end
					} else {
						if e.Current == nil {
							return "", fmt.Errorf("edit %d: single-line replace requires \"current\" (the exact content of the line being replaced)", i)
						}
						h.current, h.hasCur = *e.Current, true
					}
				case "append", "prepend":
					if e.Pos != "" {
						pos, err := parseTag(e.Pos)
						if err != nil {
							return "", err
						}
						h.pos = &pos
					}
				default:
					return "", fmt.Errorf("edit %d: unknown op %q (want replace, append, or prepend)", i, e.Op)
				}
				edits[i] = h
			}

			res, err := applyHashlineEdits(string(data), edits)
			if err != nil {
				return "", err
			}
			if res.firstChangedLine == 0 {
				msg := "No changes applied."
				if res.noopEdits > 0 {
					msg += fmt.Sprintf(" %d edit(s) were no-ops (content already matches).", res.noopEdits)
				}
				return msg, nil
			}
			if err := os.WriteFile(a.Path, []byte(res.lines), 0o644); err != nil {
				return "", err
			}
			out := fmt.Sprintf("Applied %d edit(s) to %s", len(a.Edits), a.Path)
			if res.noopEdits > 0 {
				out += fmt.Sprintf("\nNote: %d edit(s) were no-ops.", res.noopEdits)
			}
			return out, nil
		},
	}
}

func writeTool() Tool {
	return Tool{
		Def: llm.NewTool("write",
			"Write content to a file, creating it (and parent directories) or overwriting it.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"content":{"type":"string","description":"Full file content"}},"required":["path","content"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.Path), nil
		},
	}
}

func editTool() Tool {
	return Tool{
		Def: llm.NewTool("edit",
			"Replace an exact string in a file. old_string must appear exactly once unless replace_all is true.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"old_string":{"type":"string","description":"Exact text to replace"},"new_string":{"type":"string","description":"Replacement text"},"replace_all":{"type":"boolean","description":"Replace every occurrence"}},"required":["path","old_string","new_string"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			s := string(data)
			n := strings.Count(s, a.OldString)
			switch {
			case n == 0:
				return "", fmt.Errorf("old_string not found in %s", a.Path)
			case n > 1 && !a.ReplaceAll:
				return "", fmt.Errorf("old_string appears %d times in %s; make it unique or set replace_all", n, a.Path)
			}
			s = strings.ReplaceAll(s, a.OldString, a.NewString)
			if err := os.WriteFile(a.Path, []byte(s), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Replaced %d occurrence(s) in %s", n, a.Path), nil
		},
	}
}
