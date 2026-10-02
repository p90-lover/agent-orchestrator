package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

func TestCodingToolsCannotEnableLAN(testContext *testing.T) {
	previous := config.CodingToolsLocalOnly
	config.CodingToolsLocalOnly = "1"
	testContext.Cleanup(func() { config.CodingToolsLocalOnly = previous })
	manager := NewMobileLAN(http.NotFoundHandler(), 0, nil, nil)
	if port, err := manager.Start(0); err == nil || port != 0 || manager.Running() {
		testContext.Fatal("local-only build must reject LAN activation before binding")
	}
	for _, endpoint := range []string{"/api/v1/mobile/enable", "/api/v1/mobile/secure-pairing", "/api/v1/cloud/tasks", "/api/v1/sessions/test/preview/server"} {
		response := httptest.NewRecorder()
		codingToolsLocalOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			testContext.Fatal("forbidden route reached daemon handler")
		})).ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint, nil))
		if response.Code != http.StatusForbidden {
			testContext.Fatalf("%s: got %d", endpoint, response.Code)
		}
	}
}
