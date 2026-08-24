# Hashline editing (experimental)

Branch: `hashline-editing`

## What this does

Ports hashline editing (Can Bölük / oh-my-pi, packaged as
`@the-agency/pi-hashline-edit`: https://github.com/JoshMock/the-agency/tree/main/packages/hashline-edit;
writeup: https://blog.can.ac/2026/02/12/the-harness-problem/) into loopy as an
**experimental opt-in tool pair**:

- `hashline_read` — like `read`, but every line is prefixed `LINE#HASH:` where
  HASH is a 2-char tag from xxHash32 of the whitespace-stripped line (custom
  nibble alphabet `ZPMQVRWSNKTXJBYH`; blank/punctuation-only lines are seeded
  with the line number to avoid collisions).
- `hashline_edit` — edits addressed by `LINE#HASH` anchors. The hash is a
  staleness check: all anchors are validated before ANY mutation, and on
  mismatch the error returns fresh tags with `>>>` marking changed lines.
  Ops: `replace` (single line at pos, or range pos..end; single-line requires
  `current` = the expected line text, guarding right-hash-wrong-line errors),
  `append` (after pos, default EOF), `prepend` (before pos, default BOF).
  Multiple edits apply bottom-up so line numbers stay valid mid-batch.

## Gate

`~/.loopy/config.json`: `"experimental": {"hashlineEdit": true}`.

When flipped, `tui.buildAgent` swaps the agent's `read`/`edit` tools for
`hashline_read`/`hashline_edit` (`Agent.UseHashlineTools`) and the system
prompt's read/edit guidance is rewritten to teach the `LINE#HASH` workflow.
Subagents (`internal/agent/task.go`, `background.go`) and the MCP server mode
keep the classic tools (`tools.All()` is untouched).

## Non-goals

- No autocorrects from the reference (escaped `\t` fixup, range-replace
  trailing-dup trim) — magic that hides model errors. Strict validation only.
- No new dependency: xxHash32 is ~40 lines ported to Go (the reference needs
  line-number seeding for blank lines anyway, which no off-the-shelf lib does).
- Classic `read`/`edit` output format unchanged when the flag is off.

## Files

- `internal/tools/hashline.go` (new) — pure core: `xxhash32`, `computeLineHash`,
  `formatHashLines`, `parseTag`, `applyHashlineEdits`, `HashlineMismatchError`.
- `internal/tools/tools.go` — `hashlineReadTool`, `hashlineEditTool`,
  `Hashline()` tool set; `readTool(formatter)` parametrized so both read
  variants share one body.
- `internal/tools/hashline_test.go` (new) — core unit tests (ported from
  upstream `hashline.test.ts`) + tool round-trip + mismatch error text.
- `internal/config/config.go` — `Experimental map[string]bool`.
- `internal/agent/agent.go` — `UseHashlineTools()`.
- `internal/tui/tui.go` — flag check in `buildAgent`; prompt variant.
- `cmd/loopy/main.go` — `systemPrompt(experimental bool)`.
- `docs/features.md` — section.

## Tests

- Hash vectors: determinism, whitespace-insensitivity, `\r` strip, line-seeded
  blank lines; parseTag happy/error; apply: replace/range/append/prepend/EOF/
  BOF, stale-hash mismatch (error contains fresh `>>>` tags), current-content
  mismatch pre-mutation, no-op detection, out-of-range, bottom-up multi-edit,
  dedup, empty-file creation via `create_if_missing`.
- Tool level: `Hashline()` set round-trips through `Execute` (read → parse tag
  from output → edit → re-read).
- `task check` (+ `-race`; edit path takes the existing per-path file lock via
  `toolMutationPath` — hashline_edit keeps `path` as first arg so no lock
  change is needed).

## Tasks

- [x] plan sign-off (user directed: tools + experimental flag gate)
- [x] hashline.go core + tests
- [x] tools.go wiring (tools + Hashline() set)
- [x] config flag + agent swap + prompts
- [x] toolMutationPath covers hashline_edit (per-path lock)
- [x] docs/features.md
- [x] task check green (touched pkgs; TestSessionCost + shell_test cd
      failures pre-exist on main, unrelated)

Deviations: `readRun(bool)` shares one body between read/hashline_read instead
of a formatter param. Flag lives at `cfg.Experimental["hashlineEdit"]`;
subagents/MCP keep classic tools by design.
