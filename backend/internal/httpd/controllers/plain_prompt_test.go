package controllers_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionsAPI_PlainPromptReachesTheSpawnConfig(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions",
		`{"kind":"worker","harness":"claude-code","prompt":"hello","plainPrompt":true}`)
	if status != http.StatusCreated || !svc.lastSpawn.PlainPrompt {
		t.Fatalf("plain spawn=%+v status=%d body=%s", svc.lastSpawn, status, body)
	}

	svc = newFakeSessionService()
	srv = newSessionTestServer(t, svc)
	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/sessions",
		`{"kind":"worker","harness":"claude-code","prompt":"hello"}`)
	if status != http.StatusCreated || svc.lastSpawn.PlainPrompt {
		t.Fatalf("default spawn must keep AO's prompt: spawn=%+v status=%d body=%s", svc.lastSpawn, status, body)
	}
}

func TestSessionsAPI_PlainPromptIsRefusedForOrchestrators(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions",
		`{"kind":"orchestrator","harness":"claude-code","plainPrompt":true}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "PLAIN_PROMPT_UNSUPPORTED") || svc.lastSpawn.Harness != "" {
		t.Fatalf("orchestrator plain status=%d spawn=%+v body=%s", status, svc.lastSpawn, body)
	}
}
