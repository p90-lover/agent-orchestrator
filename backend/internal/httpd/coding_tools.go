package httpd

import (
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

func codingToolsLocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if config.CodingToolsLocalOnly == "1" {
			for _, prefix := range []string{"/api/v1/mobile", "/api/v1/cloud", "/api/v1/remotes", "/api/v1/dev"} {
				if request.URL.Path == prefix || strings.HasPrefix(request.URL.Path, prefix+"/") {
					http.Error(writer, "Remote access is disabled in Coding Tools", http.StatusForbidden)
					return
				}
			}
			if strings.HasSuffix(request.URL.Path, "/preview/server") {
				http.Error(writer, "Preview server launch is unavailable in the local-only integration", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(writer, request)
	})
}
