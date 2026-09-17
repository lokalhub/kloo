package cli

import "strings"

import "testing"

// TestClipErrCollapsesToOneLine: a subagent error reaches the log as a single
// greppable line. Provider failures arrive as multi-line bodies, and a log line
// that spans lines is not greppable alongside the surrounding step output.
func TestClipErrCollapsesToOneLine(t *testing.T) {
	got := clipErr("worker is powering on\n\nscheduler_busy\t(retry later)", 300)
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("clipErr left a newline/tab in %q", got)
	}
	if want := "worker is powering on scheduler_busy (retry later)"; got != want {
		t.Errorf("clipErr = %q, want %q", got, want)
	}
}

// TestClipErrBoundsLengthOnARuneBoundary: an HTML error page must not flood the
// log, and truncation must not split a multi-byte rune into invalid UTF-8.
func TestClipErrBoundsLengthOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 500) // 2 bytes each: byte-slicing would split one
	got := clipErr(long, 300)
	if r := []rune(got); len(r) != 301 { // 300 kept + the ellipsis
		t.Errorf("clipErr kept %d runes, want 301", len(r))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("clipErr did not mark the value as truncated")
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("clipErr split a rune at byte %d", i)
		}
	}
	if short := clipErr("boom", 300); short != "boom" {
		t.Errorf("clipErr truncated a short error: %q", short)
	}
}
