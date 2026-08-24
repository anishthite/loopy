// Hashline edit mode — line-addressable editing with staleness-checked
// anchors. Ported from oh-my-pi's hashline mode via the standalone
// @the-agency/pi-hashline-edit package (MIT, Can Bölük):
// https://github.com/JoshMock/the-agency/tree/main/packages/hashline-edit
//
// Each line is identified by its 1-indexed line number and a 2-char hash of
// the whitespace-normalized line text (xxHash32 over the nibble alphabet
// "ZPMQVRWSNKTXJBYH"). The combined "LINE#HASH" reference is both an address
// and a staleness check: if the file changed since the model last read it,
// hash mismatches are caught before any mutation, and the error reports the
// fresh tags so the model can self-correct in one turn.
//
// Displayed format (hashline_read): "LINENUM#HASH:TEXT"
// Reference format (hashline_edit): "LINE#HASH" (e.g. "5#ZZ")
package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// xxHash32 — pure-Go port of the 32-bit xxHash spec (seed variant), matching
// the reference implementation byte for byte. Lines are short, so the 16-byte
// block path rarely runs, but it's kept for spec correctness.
func xxhash32(s string, seed uint32) uint32 {
	const (
		p1 = 0x9e3779b1
		p2 = 0x85ebca77
		p3 = 0xc2b2ae3d
		p4 = 0x27d4eb2f
		p5 = 0x165667b1
	)
	rotl := func(x uint32, r uint) uint32 { return x<<r | x>>(32-r) }
	data := []byte(s)
	n := len(data)
	var h uint32
	i := 0
	if n >= 16 {
		v1, v2, v3, v4 := seed+p1+p2, seed+p2, seed, seed-p1
		mix := func(v uint32) uint32 {
			w := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
			i += 4
			return rotl(v+w*p2, 13) * p1
		}
		for i <= n-16 {
			v1, v2, v3, v4 = mix(v1), mix(v2), mix(v3), mix(v4)
		}
		h = rotl(v1, 1) + rotl(v2, 7) + rotl(v3, 12) + rotl(v4, 18)
	} else {
		h = seed + p5
	}
	h += uint32(n)
	for i <= n-4 {
		w := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		h = rotl(h^w*p3, 17) * p4
		i += 4
	}
	for i < n {
		h = rotl(h^uint32(data[i])*p5, 11) * p1
		i++
	}
	h ^= h >> 15
	h *= p2
	h ^= h >> 13
	h *= p3
	h ^= h >> 16
	return h
}

const nibbleStr = "ZPMQVRWSNKTXJBYH"

var reSignificant = regexp.MustCompile(`[\p{L}\p{N}]`)

// computeLineHash returns the 2-char hash tag for a line. The text is
// whitespace-normalized so re-indentation doesn't invalidate anchors. Lines
// with no alphanumeric characters mix the line number in as the seed so
// blank/punctuation-only lines don't all share one hash.
func computeLineHash(idx int, line string) string {
	line = strings.TrimSuffix(line, "\r")
	line = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, line)
	var seed uint32
	if !reSignificant.MatchString(line) {
		seed = uint32(idx)
	}
	h := xxhash32(line, seed) & 0xff
	return string([]byte{nibbleStr[h>>4], nibbleStr[h&0x0f]})
}

// formatHashLines renders text with "LINE#HASH:" prefixes (1-indexed,
// starting at startLine). Hashes are computed against the real line numbers
// so windowed reads compose with edits.
func formatHashLines(text string, startLine int) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, line := range lines {
		num := startLine + i
		fmt.Fprintf(&b, "%d#%s:%s\n", num, computeLineHash(num, line), line)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// anchor is a parsed "LINE#HASH" reference.
type anchor struct {
	line int
	hash string
}

var reTag = regexp.MustCompile(`^\s*[>+-]*\s*(\d+)\s*#\s*([ZPMQVRWSNKTXJBYH]{2})`)

// parseTag parses a line reference like "5#ZP" (tolerant of the model
// echoing back diff markers or the display format's leading/trailing text).
func parseTag(ref string) (anchor, error) {
	m := reTag.FindStringSubmatch(ref)
	if m == nil {
		return anchor{}, fmt.Errorf("invalid line reference %q — expected format \"LINE#ID\" (e.g. \"5#ZZ\")", ref)
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 {
		return anchor{}, fmt.Errorf("line number must be >= 1, got %d in %q", n, ref)
	}
	return anchor{line: n, hash: m[2]}, nil
}

// outOfRangeError reports an anchor pointing past the end of the file.
type outOfRangeError struct{ line, total int }

func (e outOfRangeError) Error() string {
	return fmt.Sprintf("line %d does not exist (file has %d lines)", e.line, e.total)
}

// hashlineEdit is one edit operation against a file's lines.
type hashlineEdit struct {
	op      string // "replace", "append", "prepend"
	pos     *anchor
	end     *anchor // range end for replace
	lines   []string
	current string // expected line content for single-line replace
	hasCur  bool
}

// hashMismatch records one anchor whose hash no longer matches the file.
type hashMismatch struct {
	line             int
	expected, actual string
}

// HashlineMismatchError is returned when one or more anchors fail validation.
// Its message shows the current file region around every mismatch with fresh
// LINE#HASH tags (mismatched lines marked >>>) so the model can retry with
// corrected references instead of re-reading the file.
type HashlineMismatchError struct {
	mismatches []hashMismatch
	fileLines  []string
}

func (e *HashlineMismatchError) Error() string {
	const ctx = 2
	show := map[int]bool{}
	bad := map[int]hashMismatch{}
	for _, m := range e.mismatches {
		bad[m.line] = m
		for i := max(1, m.line-ctx); i <= min(len(e.fileLines), m.line+ctx); i++ {
			show[i] = true
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d line(s) have changed since last read. Use the updated LINE#ID references shown below (>>> marks changed lines).\n\n", len(e.mismatches))
	prev := -1
	for i := 1; i <= len(e.fileLines); i++ {
		if !show[i] {
			continue
		}
		if prev != -1 && i > prev+1 {
			b.WriteString("    ...\n")
		}
		prev = i
		text := e.fileLines[i-1]
		tag := fmt.Sprintf("%d#%s", i, computeLineHash(i, text))
		if _, isBad := bad[i]; isBad {
			fmt.Fprintf(&b, ">>> %s:%s\n", tag, text)
		} else {
			fmt.Fprintf(&b, "    %s:%s\n", tag, text)
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// hashlineResult is the outcome of applying a batch of edits.
type hashlineResult struct {
	lines            string
	firstChangedLine int // 0 = nothing changed
	noopEdits        int
}

// applyHashlineEdits validates every anchor against the current text, then
// applies the edits bottom-up so earlier splices don't shift later targets.
// It is atomic on validation: any mismatch aborts before mutation.
func applyHashlineEdits(text string, edits []hashlineEdit) (hashlineResult, error) {
	if len(edits) == 0 {
		return hashlineResult{lines: text}, nil
	}
	fileLines := strings.Split(text, "\n")
	original := make([]string, len(fileLines))
	copy(original, fileLines)

	// pre-validate all hashes and current-content checks before mutating
	var mismatches []hashMismatch
	var currentBad []string
	validate := func(a *anchor) (bool, error) {
		if a.line < 1 || a.line > len(fileLines) {
			// out-of-range aborts the batch immediately rather than being
			// collected like hash mismatches — the remap display assumes the
			// line exists.
			return false, outOfRangeError{a.line, len(fileLines)}
		}
		actual := computeLineHash(a.line, fileLines[a.line-1])
		if actual == a.hash {
			return true, nil
		}
		mismatches = append(mismatches, hashMismatch{a.line, a.hash, actual})
		return false, nil
	}
	for i := range edits {
		e := &edits[i]
		switch e.op {
		case "replace":
			if e.end != nil {
				okp, err := validate(e.pos)
				if err != nil {
					return hashlineResult{}, err
				}
				oke, err := validate(e.end)
				if err != nil {
					return hashlineResult{}, err
				}
				if !okp || !oke {
					continue
				}
				if e.pos.line > e.end.line {
					return hashlineResult{}, fmt.Errorf("range start line %d must be <= end line %d", e.pos.line, e.end.line)
				}
			} else {
				ok, err := validate(e.pos)
				if err != nil {
					return hashlineResult{}, err
				}
				if !ok {
					continue
				}
				if e.hasCur && fileLines[e.pos.line-1] != e.current {
					currentBad = append(currentBad,
						fmt.Sprintf("  line %d: expected %s, got %s", e.pos.line, strconv.Quote(e.current), strconv.Quote(fileLines[e.pos.line-1])))
				}
			}
		case "append", "prepend":
			if e.pos != nil {
				ok, err := validate(e.pos)
				if err != nil {
					return hashlineResult{}, err
				}
				if !ok {
					continue
				}
			}
			if len(e.lines) == 0 {
				e.lines = []string{""}
			}
		}
	}
	if len(mismatches) > 0 {
		return hashlineResult{}, &HashlineMismatchError{mismatches, fileLines}
	}
	if len(currentBad) > 0 {
		return hashlineResult{}, fmt.Errorf("current content mismatch on %d replace edit(s):\n%s", len(currentBad), strings.Join(currentBad, "\n"))
	}

	// deduplicate identical edits targeting the same location
	seen := map[string]bool{}
	deduped := edits[:0]
	for _, e := range edits {
		var key string
		switch e.op {
		case "replace":
			if e.end != nil {
				key = fmt.Sprintf("r:%d:%d", e.pos.line, e.end.line)
			} else {
				key = fmt.Sprintf("s:%d", e.pos.line)
			}
		case "append":
			if e.pos != nil {
				key = fmt.Sprintf("i:%d", e.pos.line)
			} else {
				key = "ieof"
			}
		case "prepend":
			if e.pos != nil {
				key = fmt.Sprintf("ib:%d", e.pos.line)
			} else {
				key = "ibof"
			}
		}
		key += ":" + strings.Join(e.lines, "\n")
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, e)
		}
	}
	edits = deduped

	// sort bottom-up (highest line first) so splices don't shift later
	// targets; append before prepend at the same line; stable otherwise
	type annotated struct {
		e          hashlineEdit
		idx        int
		sortLine   int
		precedence int
	}
	ann := make([]annotated, len(edits))
	for i, e := range edits {
		var sl, prec int
		switch e.op {
		case "replace":
			if e.end != nil {
				sl = e.end.line
			} else {
				sl = e.pos.line
			}
		case "append":
			prec = 1
			if e.pos != nil {
				sl = e.pos.line
			} else {
				sl = len(fileLines) + 1
			}
		case "prepend":
			prec = 2
			if e.pos != nil {
				sl = e.pos.line
			}
		}
		ann[i] = annotated{e, i, sl, prec}
	}
	sort.SliceStable(ann, func(i, j int) bool {
		if ann[i].sortLine != ann[j].sortLine {
			return ann[i].sortLine > ann[j].sortLine
		}
		if ann[i].precedence != ann[j].precedence {
			return ann[i].precedence < ann[j].precedence
		}
		return ann[i].idx < ann[j].idx
	})

	res := hashlineResult{}
	changed := func(line int) {
		if res.firstChangedLine == 0 || line < res.firstChangedLine {
			res.firstChangedLine = line
		}
	}
	for _, a := range ann {
		e := a.e
		switch e.op {
		case "replace":
			if e.end == nil {
				orig := original[e.pos.line-1]
				noop := len(e.lines) == 0 && orig == "" ||
					len(e.lines) == 1 && orig == e.lines[0]
				if noop {
					res.noopEdits++
					break
				}
				fileLines = splice(fileLines, e.pos.line-1, 1, e.lines)
				changed(e.pos.line)
			} else {
				fileLines = splice(fileLines, e.pos.line-1, e.end.line-e.pos.line+1, e.lines)
				changed(e.pos.line)
			}
		case "append":
			if len(e.lines) == 0 {
				res.noopEdits++
				break
			}
			at := len(fileLines)
			if e.pos != nil {
				at = e.pos.line
			} else if len(fileLines) == 1 && fileLines[0] == "" {
				fileLines = splice(fileLines, 0, 1, e.lines)
				changed(1)
				break
			}
			fileLines = splice(fileLines, at, 0, e.lines)
			changed(at + 1)
		case "prepend":
			if len(e.lines) == 0 {
				res.noopEdits++
				break
			}
			at := 0
			if e.pos != nil {
				at = e.pos.line - 1
			}
			fileLines = splice(fileLines, at, 0, e.lines)
			changed(at + 1)
		}
	}
	res.lines = strings.Join(fileLines, "\n")
	return res, nil
}

func splice(lines []string, at, delete int, ins []string) []string {
	out := make([]string, 0, len(lines)-delete+len(ins))
	out = append(out, lines[:at]...)
	out = append(out, ins...)
	out = append(out, lines[at+delete:]...)
	return out
}
