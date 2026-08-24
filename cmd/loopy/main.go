// loopy is a minimal coding agent harness.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/context-labs/loopy/internal/agent"
	"github.com/context-labs/loopy/internal/config"
	"github.com/context-labs/loopy/internal/llm"
	"github.com/context-labs/loopy/internal/tui"
)

var version = "dev" // set via -ldflags "-X main.version=..."

func systemPrompt(experimental bool) string {
	wd, _ := os.Getwd()
	readEdit := `- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement`
	guide := `- Use bash for file operations like ls, rg, find
- Use read to examine files instead of cat or sed
- Use edit for precise changes (old_string must match exactly and be unique, or set replace_all)
- Use write only for new files or complete rewrites`
	if experimental {
		readEdit = `- hashline_read: Read file contents; every line is prefixed LINE#HASH (e.g. "5#ZP:  const x = 1;")
- bash: Execute bash commands (ls, grep, find, etc.)
- hashline_edit: Edit lines by LINE#HASH reference — replace/append/prepend, batched and validated atomically`
		guide = `- Use bash for file operations like ls, rg, find
- Use hashline_read to examine files instead of cat or sed
- Use hashline_edit for changes: copy the LINE#HASH tag(s) from hashline_read output; a single-line replace must include "current" (the line's exact text); batch multiple edits in one call; a hash mismatch means the file changed — re-read and retry with the fresh tags from the error
- Use write only for new files or complete rewrites`
	}
	prompt := fmt.Sprintf(`You are an expert coding assistant operating inside loopy, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
%s
- write: Create or overwrite files
- task: Delegate a self-contained task to a subagent with fresh context

Guidelines:
%s
- When the user tags a file with @, a note lists the tagged paths — inspect them with your tools as needed
- Be concise in your responses
- Show file paths clearly when working with files

Current working directory: %s`, readEdit, guide, wd)
	// the skills block is appended fresh each turn by the TUI, so newly added
	// skills are picked up without restarting
	return prompt
}

func main() {
	modelFlag := flag.String("m", "", "model name from ~/.loopy/config.json (default: defaultModel)")
	providerFlag := flag.String("p", "", "provider to route the model through (default: model's first provider)")
	versionFlag := flag.Bool("version", false, "print version")
	resumeFlag := flag.String("resume", "", "resume a previous session by id (or unique prefix)")
	benchFlag := flag.Bool("bench", false, "do full startup init (config, routing, key, agent) then exit; for `task benchmark`")
	flag.Parse()

	if *versionFlag {
		fmt.Println("loopy", version)
		return
	}

	// `loopy mcp ...` — server management and the MCP server mode.
	if flag.NArg() > 0 && flag.Arg(0) == "mcp" {
		if err := mcpCLI(flag.Args()[1:], version); err != nil {
			fmt.Fprintln(os.Stderr, "loopy:", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loopy:", err)
		os.Exit(1)
	}

	exp := cfg.Experimental["hashlineEdit"]
	if *benchFlag {
		prov, mdl, id, err := cfg.Resolve(*modelFlag, *providerFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "loopy:", err)
			os.Exit(1)
		}
		_ = prov.Key()
		ag := agent.New(llm.New(prov.BaseURL, "bench"), id, mdl.MaxTokens, systemPrompt(exp))
		if exp {
			ag.UseHashlineTools()
		}
		return
	}
	sessionID, err := tui.Run(cfg, *modelFlag, *providerFlag, systemPrompt(exp), *resumeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loopy:", err)
		os.Exit(1)
	}
	if sessionID != "" {
		fmt.Printf("session %s — resume with: loopy --resume %s\n", sessionID, sessionID)
	}
}
