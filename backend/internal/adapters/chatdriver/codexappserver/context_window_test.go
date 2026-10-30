package codexappserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestContextWindowIsMergedIntoThreadStartAndResume(t *testing.T) {
	for _, resume := range []bool{false, true} {
		d, srv := newTestDriver(t)
		var conv ports.ChatConversation
		var err error
		method := "thread/start"
		if resume {
			method = "thread/resume"
			conv, err = d.Resume(context.Background(), ports.ChatResumeConfig{
				SessionID: "ctx-resume", WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1",
				Model: "gpt-5.5(high)", Effort: "low", ContextWindow: 32768,
			})
		} else {
			conv, err = d.Start(context.Background(), ports.ChatStartConfig{
				SessionID: "ctx-start", WorkspacePath: t.TempDir(),
				Model: "gpt-5.5(high)", Effort: "low", ContextWindow: 32768,
			})
		}
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		request := srv.awaitFrame(func(f frame) bool { return f.Method == method })
		var params struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.Config["model_context_window"] != float64(32768) || params.Config["model_reasoning_effort"] != "low" {
			t.Fatalf("%s config=%+v", method, params.Config)
		}
		if err := conv.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
