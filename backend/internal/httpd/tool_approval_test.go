package httpd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type toolApprovalFake struct {
	session domain.SessionID
	request ports.ChatToolApprovalRequest
	result  ports.ChatToolApprovalResult
	err     error
}

func (f *toolApprovalFake) RequestToolApproval(_ context.Context, id domain.SessionID, request ports.ChatToolApprovalRequest) (ports.ChatToolApprovalResult, error) {
	f.session, f.request = id, request
	return f.result, f.err
}

func postToolApproval(r http.Handler, host, origin, auth string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://"+host+"/internal/chat/sessions/chat-1/tool-approval",
		strings.NewReader(`{"toolCallId":"call-1","toolName":"bash","kind":"execute","title":"ls","input":{"command":"ls"}}`))
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request)
	return recorder
}

func TestToolApprovalRouteAsksTheSessionsChat(t *testing.T) {
	fake := &toolApprovalFake{result: ports.ChatToolApprovalResult{Message: "The reviewer denied this"}}
	r := chi.NewRouter()
	mountToolApproval(r, fake)

	response := postToolApproval(r, "127.0.0.1", "", "Bearer secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body)
	}
	var answer ToolApprovalResponse
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || answer.Allow || answer.Message != "The reviewer denied this" {
		t.Fatalf("answer = %s (%v)", response.Body, err)
	}
	if fake.session != "chat-1" || fake.request.Token != "secret" || fake.request.Kind != "execute" ||
		fake.request.Title != "ls" || string(fake.request.Input) != `{"command":"ls"}` {
		t.Fatalf("service got %q %#v", fake.session, fake.request)
	}
}

func TestToolApprovalRouteRefusesWhatIsNotTheSessionsAgent(t *testing.T) {
	for _, test := range []struct {
		name, host, origin, auth string
		err                      error
		want                     int
	}{
		{name: "another host", host: "evil.example", auth: "Bearer secret", want: http.StatusForbidden},
		{name: "a browser page", host: "127.0.0.1", origin: "http://127.0.0.1:3000", auth: "Bearer secret", want: http.StatusForbidden},
		{name: "no token", host: "127.0.0.1", want: http.StatusUnauthorized},
		{name: "a wrong token", host: "127.0.0.1", auth: "Bearer guess", err: ports.ErrChatToolApprovalToken, want: http.StatusForbidden},
		{name: "no live chat", host: "127.0.0.1", auth: "Bearer secret", err: fmt.Errorf("%w: tool approvals", ports.ErrChatUnsupported), want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &toolApprovalFake{err: test.err, result: ports.ChatToolApprovalResult{Allowed: true}}
			r := chi.NewRouter()
			mountToolApproval(r, fake)
			response := postToolApproval(r, test.host, test.origin, test.auth)
			if response.Code != test.want || strings.Contains(response.Body.String(), `"allow":true`) {
				t.Fatalf("status = %d, body %s; want %d and no allow", response.Code, response.Body, test.want)
			}
		})
	}
}
