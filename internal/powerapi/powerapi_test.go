package powerapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	return New("s3cret", []string{"p1"}, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(s *Server, token, load, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/loads/"+load+"/power", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestPowerRequestRoundTrip(t *testing.T) {
	s := testServer(t)
	if got := s.Desired(); len(got) != 0 {
		t.Fatalf("fresh server has requests: %+v", got)
	}
	if w := do(s, "s3cret", "p1", `{"desired":"off","reason":"solar"}`); w.Code != http.StatusNoContent {
		t.Fatalf("put = %d: %s", w.Code, w.Body)
	}
	if got := s.Desired()["p1"]; got != control.RequestOff {
		t.Errorf("desired = %v, want off", got)
	}
	// "hold" is its own request - it must not read back as "nobody asked", or
	// nut-dog powers the load on by itself.
	do(s, "s3cret", "p1", `{"desired":"hold"}`)
	if got := s.Desired()["p1"]; got != control.RequestHold {
		t.Errorf("desired = %v, want hold", got)
	}
	if control.RequestHold == control.NoRequest {
		t.Error("hold and no-request must stay distinct values")
	}
}

// The returned map must not alias the server's, or a caller iterating it while a
// request lands races the reconcile.
func TestDesiredIsACopy(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	got := s.Desired()
	got["p1"] = control.RequestOn
	if s.Desired()["p1"] != control.RequestOff {
		t.Error("mutating the returned map changed the server's state")
	}
}

func TestPowerRequestRejects(t *testing.T) {
	for _, tc := range []struct {
		name, token, load, body string
		want                    int
	}{
		{"no token", "", "p1", `{"desired":"off"}`, http.StatusUnauthorized},
		{"wrong token", "nope", "p1", `{"desired":"off"}`, http.StatusUnauthorized},
		{"unknown load", "s3cret", "bc9", `{"desired":"off"}`, http.StatusNotFound},
		{"unknown desired", "s3cret", "p1", `{"desired":"maybe"}`, http.StatusBadRequest},
		{"not json", "s3cret", "p1", `off`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			if w := do(s, tc.token, tc.load, tc.body); w.Code != tc.want {
				t.Errorf("code = %d, want %d", w.Code, tc.want)
			}
			if len(s.Desired()) != 0 {
				t.Error("a rejected request still changed state")
			}
		})
	}
}

// A caller that dies must not pin a load forever. Expiry hands it back to the UPSes - which
// is a power-ON on healthy sources, not a neutral state, so the drop has to be deliberate.
func TestRequestExpires(t *testing.T) {
	s := testServer(t)
	base := time.Now()
	s.now = func() time.Time { return base }

	do(s, "s3cret", "p1", `{"desired":"off"}`)
	if got := s.Desired()["p1"]; got != control.RequestOff {
		t.Fatalf("desired = %v, want off", got)
	}

	s.now = func() time.Time { return base.Add(59 * time.Minute) }
	if _, ok := s.Desired()["p1"]; !ok {
		t.Error("request dropped before its TTL")
	}

	s.now = func() time.Time { return base.Add(time.Hour) }
	if got, ok := s.Desired()["p1"]; ok {
		t.Errorf("request survived its TTL: %v", got)
	}
}

// Restating refreshes the clock, so a live caller never expires. energy-watchdog restates
// every tick even when it cannot see, which is what keeps a blind watchdog from tripping it.
func TestRestatingKeepsTheRequestAlive(t *testing.T) {
	s := testServer(t)
	base := time.Now()
	for i := range 4 {
		s.now = func() time.Time { return base.Add(time.Duration(i) * 30 * time.Minute) }
		do(s, "s3cret", "p1", `{"desired":"off"}`)
		if _, ok := s.Desired()["p1"]; !ok {
			t.Fatalf("request expired at +%dm despite being restated", i*30)
		}
	}
}

// A TTL of zero means never expire.
func TestZeroTTLNeverExpires(t *testing.T) {
	s := New("s3cret", []string{"p1"}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	base := time.Now()
	s.now = func() time.Time { return base }
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	s.now = func() time.Time { return base.Add(30 * 24 * time.Hour) }
	if _, ok := s.Desired()["p1"]; !ok {
		t.Error("request expired with the TTL disabled")
	}
}
