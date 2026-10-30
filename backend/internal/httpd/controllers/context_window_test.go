package controllers_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionsAPI_ContextWindowReachesTheSpawnConfig(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions",
		`{"kind":"worker","harness":"opencode","model":"openai/gpt-5.5(high)","contextWindow":32768,"gateway":{"provider":"cpa","model":"gpt-5.5(high)"}}`)
	if status != http.StatusCreated || svc.lastSpawn.AgentConfig.ContextWindow != 32768 ||
		svc.lastSpawn.Gateway.Model != "gpt-5.5(high)" {
		t.Fatalf("context spawn=%+v status=%d body=%s", svc.lastSpawn, status, body)
	}
}

func TestSessionsAPI_UnsupportedTuningFailsBeforeSpawn(t *testing.T) {
	for _, payload := range []string{
		`{"harness":"agy","contextWindow":32768}`,
		`{"harness":"codex","contextWindow":-1}`,
		`{"harness":"opencode","model":"openai/gpt-5.5","effort":"high"}`,
		`{"harness":"pi","effort":"high"}`,
		`{"harness":"claude-code","model":"claude-sonnet-4-5","contextWindow":32768}`,
	} {
		svc := newFakeSessionService()
		srv := newSessionTestServer(t, svc)
		body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", payload)
		if status != http.StatusBadRequest || !strings.Contains(string(body), "UNSUPPORTED_") || svc.lastSpawn.Harness != "" {
			t.Errorf("unsupported tuning status=%d spawn=%+v body=%s", status, svc.lastSpawn, body)
		}
	}
}
