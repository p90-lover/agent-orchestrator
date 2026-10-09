package store_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// plain_prompt is pinned at spawn and read back by every full-session query, so a
// restored or listed plain session still starts without AO's system prompt.
func TestPlainPromptRoundTripsThroughEverySessionRead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "plain")
	want := map[domain.SessionID]bool{}
	for _, plain := range []bool{true, false} {
		rec := sampleRecord("plain")
		rec.Metadata.PlainPrompt = plain
		row, err := s.CreateSession(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		want[row.ID] = plain
		got, ok, err := s.GetSession(ctx, row.ID)
		if err != nil || !ok || got.Metadata.PlainPrompt != plain {
			t.Fatalf("get plain=%v: got %v ok=%v err=%v", plain, got.Metadata.PlainPrompt, ok, err)
		}
	}
	listed, err := s.ListSessions(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.ListAllSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]domain.SessionRecord{listed, all} {
		seen := 0
		for _, rec := range rows {
			if plain, ok := want[rec.ID]; ok {
				seen++
				if rec.Metadata.PlainPrompt != plain {
					t.Fatalf("list %s: PlainPrompt=%v want %v", rec.ID, rec.Metadata.PlainPrompt, plain)
				}
			}
		}
		if seen != len(want) {
			t.Fatalf("listed %d of %d sessions", seen, len(want))
		}
	}
}
