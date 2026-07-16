package runner

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// TestStreamTail_CapsEventCount proves the last-200-events cap: past the cap,
// the head lines are dropped and the tail (highest-index lines) survive intact.
func TestStreamTail_CapsEventCount(t *testing.T) {
	var stdout bytes.Buffer
	const n = maxStreamTailEvents + 50
	for i := 0; i < n; i++ {
		stdout.WriteString(`{"seq":` + strconv.Itoa(i) + "}\n")
	}

	got := streamTail(stdout.Bytes())
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(lines) != maxStreamTailEvents {
		t.Fatalf("len(lines) = %d, want %d", len(lines), maxStreamTailEvents)
	}
	if want := `{"seq":50}`; lines[0] != want {
		t.Errorf("first surviving line = %q, want %q (the head must be dropped)", lines[0], want)
	}
	if want := `{"seq":` + strconv.Itoa(n-1) + `}`; lines[len(lines)-1] != want {
		t.Errorf("last surviving line = %q, want %q (the tail must survive)", lines[len(lines)-1], want)
	}
}

// TestStreamTail_CapsByteSize proves the 64KB cap trims further from the head
// even when the event count is well under maxStreamTailEvents.
func TestStreamTail_CapsByteSize(t *testing.T) {
	var stdout bytes.Buffer
	const lineSize = 10 << 10 // 10KB per line, 10 lines = 100KB > 64KB cap
	for i := 0; i < 10; i++ {
		stdout.WriteString(`{"seq":` + strconv.Itoa(i) + `,"pad":"` + strings.Repeat("a", lineSize) + "\"}\n")
	}

	got := streamTail(stdout.Bytes())
	if len(got) > maxStreamTailBytes {
		t.Fatalf("len(tail) = %d, want <= %d", len(got), maxStreamTailBytes)
	}
	if !bytes.Contains(got, []byte(`"seq":9`)) {
		t.Error("tail dropped the last event; the byte cap must trim the head, not the tail")
	}
	if bytes.Contains(got, []byte(`"seq":0,`)) {
		t.Error("tail kept the first event; the byte cap should have dropped it")
	}
}

// TestStreamTail_Empty proves an empty or whitespace-only stream yields no file
// content, never a panic or a spurious event.
func TestStreamTail_Empty(t *testing.T) {
	if got := streamTail(nil); len(got) != 0 {
		t.Errorf("streamTail(nil) = %q, want empty", got)
	}
	if got := streamTail([]byte("\n\n  \n")); len(got) != 0 {
		t.Errorf("streamTail(blank) = %q, want empty", got)
	}
}

// TestStreamTail_SingleHugeLineNeverEmpty proves the rolling tail never goes
// silently empty: even one line alone larger than the byte cap still yields a
// non-empty, bounded suffix rather than nothing.
func TestStreamTail_SingleHugeLineNeverEmpty(t *testing.T) {
	huge := []byte(`{"seq":0,"pad":"` + strings.Repeat("a", maxStreamTailBytes*2) + `"}` + "\n")

	got := streamTail(huge)
	if len(got) == 0 {
		t.Fatal("streamTail of one oversized line = empty, want a non-empty bounded suffix")
	}
	if len(got) > maxStreamTailBytes {
		t.Errorf("len(tail) = %d, want <= %d", len(got), maxStreamTailBytes)
	}
}
