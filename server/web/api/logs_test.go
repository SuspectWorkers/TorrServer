package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"server/log"
)

func TestGetLogsEndpointPlainAndDownloadDiag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log.Init("", "")
	defer log.Close()
	log.TLogln("api-logs-probe-entry")

	router := gin.New()
	router.GET("/logs", getLogs)

	req := httptest.NewRequest(http.MethodGet, "/logs?tail=65536", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "api-logs-probe-entry") {
		t.Fatalf("expected log line in response, got %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "TORRSERVER DIAGNOSTICS") {
		t.Fatalf("plain /logs should not include diagnostics header unless diag=1")
	}

	reqDl := httptest.NewRequest(http.MethodGet, "/logs?download=1", nil)
	wDl := httptest.NewRecorder()
	router.ServeHTTP(wDl, reqDl)

	if wDl.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", wDl.Code)
	}
	cd := wDl.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment;") || !strings.Contains(cd, "torrserver-debug-") {
		t.Fatalf("unexpected Content-Disposition: %q", cd)
	}
	body := wDl.Body.String()
	if !strings.Contains(body, "TORRSERVER DIAGNOSTICS") || !strings.Contains(body, "api-logs-probe-entry") {
		t.Fatalf("expected diagnostics + log entry in download body, got %q", body)
	}
}
