package chat

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestNativeHistoryBatchEnd(t *testing.T) {
	small := make([]ports.ChatEvent, 300)
	if got := batchEnds(small); !equalInts(got, []int{128, 256, 300}) {
		t.Fatalf("300 small events split at %v, want [128 256 300]", got)
	}

	// Large events end a batch at the byte budget, and one larger than the whole
	// budget still goes in, alone.
	three := strings.Repeat("x", 3<<20)
	large := []ports.ChatEvent{{Text: "a"}, {Detail: []byte(three)}, {Text: three}, {Delta: strings.Repeat("y", 10<<20)}, {Summary: "b"}}
	if got := batchEnds(large); !equalInts(got, []int{2, 3, 4, 5}) {
		t.Fatalf("large events split at %v, want [2 3 4 5]", got)
	}

	if got := batchEnds(nil); len(got) != 0 {
		t.Fatalf("no events split at %v, want none", got)
	}
}

func batchEnds(events []ports.ChatEvent) []int {
	var ends []int
	for start := 0; start < len(events); {
		start = nativeHistoryBatchEnd(events, start)
		ends = append(ends, start)
	}
	return ends
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
