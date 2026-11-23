package claudeacp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const testClaudeSession = "8ed9ebb8-7b75-4e14-a5a8-359b081dc372"

var testClaudeFailedAt = time.Date(2026, 11, 23, 10, 5, 21, 151_000_000, time.UTC)

// testAPIErrorRecord is the record Claude Code 2.1.322 wrote on 2026-11-23 for a
// Vertex 403 behind the CPA gateway, trimmed to the fields that matter.
func testAPIErrorRecord(status int, text string, at time.Time) map[string]any {
	record := map[string]any{
		"parentUuid": "11111111-1111-4111-8111-111111111111", "isSidechain": false, "type": "assistant",
		"uuid": "22222222-2222-4222-8222-222222222222", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{
			"id": "msg", "model": "<synthetic>", "role": "assistant", "type": "message", "stop_reason": "stop_sequence",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
		"error": "authentication_failed", "isApiErrorMessage": true, "sessionId": testClaudeSession,
	}
	if status != 0 {
		record["apiErrorStatus"] = status
	}
	return record
}

func testAnswerRecord(at time.Time, sidechain bool) map[string]any {
	return map[string]any{
		"type": "assistant", "isSidechain": sidechain, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"model": "gemini-4-carbon", "content": []any{map[string]any{"type": "text", "text": "working on it"}}},
	}
}

func writeTestTranscript(t *testing.T, path string, records ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, record := range records {
		if line, ok := record.(string); ok {
			lines = append(lines, line)
			continue
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testAuthContext(t *testing.T) (acpdriver.AuthRequiredContext, string) {
	t.Helper()
	configDir := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "Projects", "coding-tools-mcp")
	key := claudeProjectKeyUnsafe.ReplaceAllString(workspace, "-")
	return acpdriver.AuthRequiredContext{
		SessionID: testClaudeSession, WorkspacePath: workspace,
		Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir},
	}, filepath.Join(configDir, "projects", key, testClaudeSession+".jsonl")
}

const testBillingRefusal = "Failed to authenticate. API Error: 403 Access requires an eligible paid billing account in a supported region, subject to our terms and policies."

func TestClaudeAuthRequiredFailureReadsTheProviderRefusal(t *testing.T) {
	now := func() time.Time { return testClaudeFailedAt.Add(time.Second) }
	noSleep := func(time.Duration) {}
	for _, tc := range []struct {
		name    string
		records []any
		want    string // "" means: keep the auth failure
	}{
		{
			name:    "403 from the provider",
			records: []any{testAnswerRecord(testClaudeFailedAt.Add(-time.Minute), false), testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt)},
			want:    "API Error: 403 Access requires an eligible paid billing account in a supported region, subject to our terms and policies.",
		},
		{
			name:    "status only in the text",
			records: []any{testAPIErrorRecord(0, "API Error: 403 Forbidden", testClaudeFailedAt)},
			want:    "API Error: 403 Forbidden",
		},
		{
			name:    "a real 401 stays a sign-in failure",
			records: []any{testAPIErrorRecord(401, "Failed to authenticate. API Error: 401 Invalid API key", testClaudeFailedAt)},
		},
		{
			name:    "a sign-in instruction stays a sign-in failure",
			records: []any{testAPIErrorRecord(403, "OAuth token revoked · Please run /login", testClaudeFailedAt)},
		},
		{
			name:    "no status at all",
			records: []any{testAPIErrorRecord(0, "Failed to authenticate.", testClaudeFailedAt)},
		},
		{
			name:    "a later real answer means the error is not this failure's",
			records: []any{testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt.Add(-30*time.Second)), testAnswerRecord(testClaudeFailedAt, false)},
		},
		{
			name:    "an old error is not this failure's",
			records: []any{testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt.Add(-10*time.Minute))},
		},
		{
			name: "subagent records and other lines after the error are skipped",
			records: []any{
				testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt),
				testAnswerRecord(testClaudeFailedAt, true),
				map[string]any{"type": "system", "timestamp": testClaudeFailedAt.Format(time.RFC3339Nano)},
				"{not json",
			},
			want: "API Error: 403 Access requires an eligible paid billing account in a supported region, subject to our terms and policies.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc, path := testAuthContext(t)
			writeTestTranscript(t, path, tc.records...)
			err := claudeAuthRequiredFailureAt(pc, now, noSleep)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("failure = %v, want nil (keep the auth failure)", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("failure = %v, want %q", err, tc.want)
			}
			if errors.Is(err, ports.ErrChatAuthRequired) {
				t.Fatalf("a provider refusal must not ask for a sign-in: %v", err)
			}
		})
	}
}

// The record can land a moment after the adapter refused the prompt: the hook
// looks again a few times before it gives up.
func TestClaudeAuthRequiredFailureWaitsForALateRecord(t *testing.T) {
	pc, path := testAuthContext(t)
	sleeps := 0
	err := claudeAuthRequiredFailureAt(pc, func() time.Time { return testClaudeFailedAt.Add(time.Second) }, func(time.Duration) {
		sleeps++
		if sleeps == 1 {
			writeTestTranscript(t, path, testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt))
		}
	})
	if err == nil || !strings.HasPrefix(err.Error(), "API Error: 403") || sleeps != 1 {
		t.Fatalf("failure = %v after %d waits, want the 403 after one wait", err, sleeps)
	}

	missing, _ := testAuthContext(t)
	sleeps = 0
	if err := claudeAuthRequiredFailureAt(missing, time.Now, func(time.Duration) { sleeps++ }); err != nil {
		t.Fatalf("failure without a transcript = %v, want nil", err)
	}
	if sleeps != claudeAPIErrorReadAttempts-1 {
		t.Fatalf("waited %d times without a transcript, want %d", sleeps, claudeAPIErrorReadAttempts-1)
	}
}

// A transcript outside the workspace's own project folder is still found, and a
// session id that is not Claude's is never turned into a path.
func TestClaudeTranscriptPathFallsBackAndRejectsForeignIDs(t *testing.T) {
	pc, _ := testAuthContext(t)
	elsewhere := filepath.Join(pc.Env["CLAUDE_CONFIG_DIR"], "projects", "-somewhere-else", testClaudeSession+".jsonl")
	writeTestTranscript(t, elsewhere, testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt))
	if got := claudeTranscriptPath(pc, testClaudeSession); got != elsewhere {
		t.Fatalf("transcript = %q, want %q", got, elsewhere)
	}
	for _, id := range []string{"", "../" + testClaudeSession, `..\x`, "not-a-uuid"} {
		bad := pc
		bad.SessionID = id
		if err := claudeAuthRequiredFailureAt(bad, time.Now, func(time.Duration) { t.Fatal("must not wait for a foreign id") }); err != nil {
			t.Fatalf("id %q: failure = %v, want nil", id, err)
		}
	}
}

// The error record is read from the end of a long transcript, past a line far
// larger than the tail that is read.
func TestClaudeAuthRequiredFailureReadsPastAHugeEarlierLine(t *testing.T) {
	pc, path := testAuthContext(t)
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", claudeTranscriptTailBytes+1024) + `"}}`
	writeTestTranscript(t, path, huge, testAPIErrorRecord(403, testBillingRefusal, testClaudeFailedAt))
	err := claudeAuthRequiredFailureAt(pc, func() time.Time { return testClaudeFailedAt }, func(time.Duration) {})
	if err == nil || !strings.HasPrefix(err.Error(), "API Error: 403 Access requires") {
		t.Fatalf("failure = %v, want the 403", err)
	}
}
