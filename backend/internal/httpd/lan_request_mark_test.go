package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

// A request served by the LAN listener carries the LAN mark into the shared
// router, whatever its Host header says; the loopback router never sets it.
func TestLANControlBlockMarksRequestsAsLAN(t *testing.T) {
	var marked bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marked = controllers.IsLANRequest(r.Context())
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/sessions", nil)
	lanControlBlock(inner).ServeHTTP(httptest.NewRecorder(), request)
	if !marked {
		t.Fatal("a LAN-served request must be marked")
	}
	inner.ServeHTTP(httptest.NewRecorder(), request)
	if marked {
		t.Fatal("a loopback request must not be marked")
	}
}
