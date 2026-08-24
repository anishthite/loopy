package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func anchorFor(line int, text string) *anchor {
	return &anchor{line, computeLineHash(line, text)}
}

func TestComputeLineHash(t *testing.T) {
	if h := computeLineHash(1, "hello world"); len(h) != 2 {
		t.Fatalf("hash must be 2 chars, got %q", h)
	}
	if computeLineHash(1, "const x = 1;") != computeLineHash(1, "const x = 1;") {
		t.Fatal("same input must hash the same")
	}
	if computeLineHash(1, "const x = 1;") != computeLineHash(1, "const  x  =  1;") {
		t.Fatal("whitespace differences must not change the hash")
	}
	if computeLineHash(1, "hello") != computeLineHash(1, "hello\r") {
		t.Fatal("trailing CR must be stripped")
	}
	if computeLineHash(1, "---") == computeLineHash(2, "---") {
		t.Fatal("non-alphanumeric lines must be seeded by line number")
	}
}

func TestFormatHashLines(t *testing.T) {
	out := formatHashLines("a\nb\nc", 1)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	for i, want := range []string{`^1#[A-Z]{2}:a$`, `^2#[A-Z]{2}:b$`, `^3#[A-Z]{2}:c$`} {
		if !regexp.MustCompile(want).MatchString(lines[i]) {
			t.Fatalf("line %d: %q doesn't match %s", i, lines[i], want)
		}
	}
	out = formatHashLines("x\ny", 10)
	if !regexp.MustCompile(`^10#[A-Z]{2}:x`).MatchString(strings.Split(out, "\n")[0]) {
		t.Fatalf("startLine not respected: %q", out)
	}
}

func TestParseTag(t *testing.T) {
	a, err := parseTag("5#ZP")
	if err != nil || a.line != 5 || a.hash != "ZP" {
		t.Fatalf("parseTag(5#ZP) = %+v, %v", a, err)
	}
	a, err = parseTag("  10#MQ")
	if err != nil || a.line != 10 {
		t.Fatalf("leading whitespace: %+v, %v", a, err)
	}
	a, err = parseTag(">>> 12#VT:some text") // echoing the display format back
	if err != nil || a.line != 12 || a.hash != "VT" {
		t.Fatalf("display-format echo: %+v, %v", a, err)
	}
	if _, err = parseTag("invalid"); err == nil {
		t.Fatal("invalid ref must fail")
	}
	if _, err = parseTag("0#ZZ"); err == nil {
		t.Fatal("line 0 must fail")
	}
}

func TestApplyHashlineEdits(t *testing.T) {
	// no edits → unchanged
	r, err := applyHashlineEdits("hello\nworld", nil)
	if err != nil || r.lines != "hello\nworld" || r.firstChangedLine != 0 {
		t.Fatalf("empty: %+v %v", r, err)
	}

	// single-line replace
	r, err = applyHashlineEdits("aaa\nbbb\nccc", []hashlineEdit{
		{op: "replace", pos: anchorFor(2, "bbb"), lines: []string{"BBB"}, current: "bbb", hasCur: true},
	})
	if err != nil || r.lines != "aaa\nBBB\nccc" || r.firstChangedLine != 2 {
		t.Fatalf("replace: %+v %v", r, err)
	}

	// range replace
	r, err = applyHashlineEdits("aaa\nbbb\nccc\nddd", []hashlineEdit{
		{op: "replace", pos: anchorFor(2, "bbb"), end: anchorFor(3, "ccc"), lines: []string{"XXX"}},
	})
	if err != nil || r.lines != "aaa\nXXX\nddd" {
		t.Fatalf("range: %+v %v", r, err)
	}

	// append after pos / at EOF
	r, err = applyHashlineEdits("aaa\nbbb", []hashlineEdit{
		{op: "append", pos: anchorFor(1, "aaa"), lines: []string{"inserted"}},
	})
	if err != nil || r.lines != "aaa\ninserted\nbbb" {
		t.Fatalf("append: %+v %v", r, err)
	}
	r, err = applyHashlineEdits("aaa\nbbb", []hashlineEdit{
		{op: "append", lines: []string{"ccc"}},
	})
	if err != nil || r.lines != "aaa\nbbb\nccc" {
		t.Fatalf("append EOF: %+v %v", r, err)
	}

	// prepend before pos / at BOF
	r, err = applyHashlineEdits("aaa\nbbb", []hashlineEdit{
		{op: "prepend", pos: anchorFor(2, "bbb"), lines: []string{"inserted"}},
	})
	if err != nil || r.lines != "aaa\ninserted\nbbb" {
		t.Fatalf("prepend: %+v %v", r, err)
	}
	r, err = applyHashlineEdits("aaa\nbbb", []hashlineEdit{
		{op: "prepend", lines: []string{"zzz"}},
	})
	if err != nil || r.lines != "zzz\naaa\nbbb" {
		t.Fatalf("prepend BOF: %+v %v", r, err)
	}

	// stale hash → mismatch error with fresh tags
	_, err = applyHashlineEdits("aaa\nbbb", []hashlineEdit{
		{op: "replace", pos: &anchor{1, "XX"}, lines: []string{"new"}, current: "aaa", hasCur: true},
	})
	me, ok := err.(*HashlineMismatchError)
	if !ok {
		t.Fatalf("want HashlineMismatchError, got %v", err)
	}
	if !strings.Contains(me.Error(), ">>>") || !strings.Contains(me.Error(), "1#") {
		t.Fatalf("mismatch message must show fresh tags: %q", me.Error())
	}

	// no-op detection
	r, err = applyHashlineEdits("aaa\nbbb", []hashlineEdit{
		{op: "replace", pos: anchorFor(1, "aaa"), lines: []string{"aaa"}, current: "aaa", hasCur: true},
	})
	if err != nil || r.firstChangedLine != 0 || r.noopEdits != 1 {
		t.Fatalf("noop: %+v %v", r, err)
	}

	// out-of-range line
	_, err = applyHashlineEdits("aaa", []hashlineEdit{
		{op: "replace", pos: &anchor{5, "ZZ"}, lines: []string{"x"}, current: "x", hasCur: true},
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("out of range: %v", err)
	}

	// current content mismatch, caught before any mutation
	_, err = applyHashlineEdits("aaa\nbbb\nccc", []hashlineEdit{
		{op: "replace", pos: anchorFor(1, "aaa"), lines: []string{"AAA"}},
		{op: "replace", pos: anchorFor(2, "bbb"), lines: []string{"BBB"}, current: "wrong", hasCur: true},
	})
	if err == nil || !strings.Contains(err.Error(), "current content mismatch") {
		t.Fatalf("current mismatch: %v", err)
	}
	if !strings.Contains(err.Error(), `"wrong"`) || !strings.Contains(err.Error(), `"bbb"`) {
		t.Fatalf("current mismatch must show expected and actual: %q", err)
	}

	// bottom-up: two edits in one batch, higher line applied first so the
	// lower line number stays valid
	r, err = applyHashlineEdits("a\nb\nc\nd", []hashlineEdit{
		{op: "replace", pos: anchorFor(1, "a"), lines: []string{"A1", "A2"}},
		{op: "replace", pos: anchorFor(4, "d"), lines: []string{"D"}},
	})
	if err != nil || r.lines != "A1\nA2\nb\nc\nD" || r.firstChangedLine != 1 {
		t.Fatalf("bottom-up: %+v %v", r, err)
	}

	// dedup identical edits at the same location
	r, err = applyHashlineEdits("a\nb", []hashlineEdit{
		{op: "append", pos: anchorFor(1, "a"), lines: []string{"x"}},
		{op: "append", pos: anchorFor(1, "a"), lines: []string{"x"}},
	})
	if err != nil || r.lines != "a\nx\nb" {
		t.Fatalf("dedup: %+v %v", r, err)
	}

	// append into empty file
	r, err = applyHashlineEdits("", []hashlineEdit{
		{op: "append", lines: []string{"first", "second"}},
	})
	if err != nil || r.lines != "first\nsecond" {
		t.Fatalf("empty file append: %+v %v", r, err)
	}
}

// runHashline executes a tool from the Hashline set.
func runHashline(t *testing.T, name, args string) string {
	t.Helper()
	return Execute(context.Background(), Hashline(), name, json.RawMessage(args))
}

// TestHashlineToolRoundTrip drives read → parse tag from output → edit →
// re-read through the public tool surface.
func TestHashlineToolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runHashline(t, "hashline_read", fmt.Sprintf(`{"path":%q}`, f))
	if strings.HasPrefix(out, "Error") {
		t.Fatal(out)
	}
	// grab the tag for line 2
	re := regexp.MustCompile(`(?m)^2#([A-Z]{2}):two$`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("read output missing hashline tag: %q", out)
	}
	tag := "2#" + m[1]

	editArgs := fmt.Sprintf(`{"path":%q,"edits":[{"op":"replace","pos":%q,"lines":["2"],"current":"two"}]}`, f, tag)
	if out := runHashline(t, "hashline_edit", editArgs); strings.HasPrefix(out, "Error") {
		t.Fatal(out)
	}
	out = runHashline(t, "hashline_read", fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, f))
	if !strings.Contains(out, ":2") || strings.Contains(out, ":two") {
		t.Fatalf("edit not applied: %q", out)
	}

	// stale tag must fail with a remap error, not corrupt the file (a
	// third-party bash write simulates the file changing under the agent,
	// so the hash — not just the current-content check — is what trips)
	runHashline(t, "bash", fmt.Sprintf(`{"command":%q}`, "printf 'one\\nTWO\\nthree\\n' > "+f))
	if out := runHashline(t, "hashline_edit", editArgs); !strings.Contains(out, "changed since last read") || !strings.Contains(out, ">>>") {
		t.Fatalf("stale anchor must fail with remap: %q", out)
	}
	data, _ := os.ReadFile(f)
	if string(data) != "one\nTWO\nthree\n" {
		t.Fatalf("failed edit mutated the file: %q", data)
	}

	// right hash, wrong expectation of the line's content: the "current"
	// check catches a wrong mental model even when the anchor is valid
	tag2 := "2#" + computeLineHash(2, "TWO")
	out = runHashline(t, "hashline_edit", fmt.Sprintf(`{"path":%q,"edits":[{"op":"replace","pos":%q,"lines":["x"],"current":"two"}]}`, f, tag2))
	if !strings.Contains(out, "current content mismatch") {
		t.Fatalf("current mismatch must be caught: %q", out)
	}

	// create_if_missing
	g := filepath.Join(dir, "sub", "new.txt")
	if out := runHashline(t, "hashline_edit", fmt.Sprintf(`{"path":%q,"edits":[{"op":"append","lines":["hello"]}]}`, g)); !strings.Contains(out, "create_if_missing") {
		t.Fatalf("missing file must ask for create_if_missing: %q", out)
	}
	out = runHashline(t, "hashline_edit", fmt.Sprintf(`{"path":%q,"create_if_missing":true,"edits":[{"op":"append","lines":["hello","world"]}]}`, g))
	if !strings.Contains(out, "Created") {
		t.Fatalf("create: %q", out)
	}
	data, _ = os.ReadFile(g)
	if string(data) != "hello\nworld" {
		t.Fatalf("created content: %q", data)
	}

	// single-line replace without current must be rejected
	out = runHashline(t, "hashline_edit", fmt.Sprintf(`{"path":%q,"edits":[{"op":"replace","pos":%q,"lines":["x"]}]}`, f, "1#ZZ"))
	if !strings.Contains(out, "current") {
		t.Fatalf("missing current: %q", out)
	}
}
