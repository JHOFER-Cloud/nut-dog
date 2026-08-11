package powerapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 7235: the auth scheme is case-insensitive. The token is not.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	s := New("s3cret", []string{"p1"}, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, h := range []string{"Bearer s3cret", "bearer s3cret", "BEARER s3cret"} {
		req := httptest.NewRequest(http.MethodPut, "/api/loads/p1/power", strings.NewReader(`{"desired":"off"}`))
		req.Header.Set("Authorization", h)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Errorf("%q = %d, want 204", h, w.Code)
		}
	}
	for _, h := range []string{"Bearer S3CRET", "Basic s3cret", "s3cret", ""} {
		req := httptest.NewRequest(http.MethodPut, "/api/loads/p1/power", strings.NewReader(`{"desired":"off"}`))
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%q = %d, want 401", h, w.Code)
		}
	}
}
