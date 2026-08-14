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

// poll is one reconcile's worth of contact with the store: the read nut-dog acts on, then
// the acknowledgement that it did. Only the pair advances a shed's one-poll hold.
func poll(s *Server) control.Request {
	got := s.Desired()["p1"]
	s.Applied()
	return got
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
	if got := poll(s); got != control.RequestOff {
		t.Errorf("desired = %v, want off", got)
	}
	// "hold" is its own request - it must not read back as "nobody asked", or
	// nut-dog powers the load on by itself.
	do(s, "s3cret", "p1", `{"desired":"hold"}`)
	if got := poll(s); got != control.RequestHold {
		t.Errorf("desired = %v, want hold", got)
	}
	if control.RequestHold == control.NoRequest {
		t.Error("hold and no-request must stay distinct values")
	}
}

// Reading must be repeatable. The shed hold depends on exactly one reconcile per Applied,
// so a probe or a debug handler calling Desired must not consume anything.
func TestDesiredDoesNotAdvanceTheShedHold(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	for range 3 {
		if got := s.Desired()["p1"]; got != control.RequestOff {
			t.Fatalf("desired = %v, want off", got)
		}
	}
	do(s, "s3cret", "p1", `{"desired":"on"}`)
	if got := poll(s); got != control.RequestOff {
		t.Errorf("desired = %v, want off; reading it three times applied the shed", got)
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

// The production failure: energy-watchdog shed p1, then a desktop VM request 237ms later
// made it ask for p1 back. nut-dog polls every 15s, so the off was overwritten in place and
// never reached a reconcile - p1 stayed up with every guest stopped.
func TestShedIsAppliedBeforeItCanBeTakenBack(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off","reason":"deficit"}`)
	do(s, "s3cret", "p1", `{"desired":"on","reason":"desktop VM requested"}`)

	if got := poll(s); got != control.RequestOff {
		t.Fatalf("desired = %v, want off; the shed was dropped before any poll saw it", got)
	}
	if got := poll(s); got != control.RequestOn {
		t.Errorf("desired = %v, want on; the deferred request never took over", got)
	}
}

// Only a shed gets that treatment. A wake superseded before it is applied is simply gone:
// acting on it would power a load the caller has since decided it does not want up. And a
// shed is never the thing that waits - it supersedes an unapplied wake at once.
func TestOnlyAShedIsHeldForAPoll(t *testing.T) {
	for _, tc := range []struct {
		first, second string
		want          control.Request
	}{
		{"on", "hold", control.RequestHold},
		{"hold", "on", control.RequestOn},
		{"on", "off", control.RequestOff},
	} {
		t.Run(tc.first+"-then-"+tc.second, func(t *testing.T) {
			s := testServer(t)
			do(s, "s3cret", "p1", `{"desired":"`+tc.first+`"}`)
			do(s, "s3cret", "p1", `{"desired":"`+tc.second+`"}`)
			if got := poll(s); got != tc.want {
				t.Errorf("desired = %v, want %v", got, tc.want)
			}
		})
	}
}

// The caller restates its wish every tick. A restatement of the shed already in force is not
// a new shed and must not re-arm the one-poll hold, or the wake that ends a long shed is
// deferred by an extra poll for no reason.
func TestRestatingAnAppliedShedKeepsItApplied(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	poll(s)
	do(s, "s3cret", "p1", `{"desired":"off"}`) // the every-tick restate
	do(s, "s3cret", "p1", `{"desired":"on"}`)

	if got := poll(s); got != control.RequestOn {
		t.Errorf("desired = %v, want on; the restate put the shed back into unapplied", got)
	}
}

// Once a poll has read the shed it is an ordinary standing request again, so the wake that
// follows lands on the very next poll rather than a second one.
func TestAnAppliedShedIsSupersededAtOnce(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	poll(s)

	do(s, "s3cret", "p1", `{"desired":"on"}`)
	if got := poll(s); got != control.RequestOn {
		t.Errorf("desired = %v, want on", got)
	}
}

// Restating the shed while it is still unapplied must not park a deferred copy of it. The
// poll in the middle is what makes this bite: a shed deferred behind itself is never marked
// applied, so the wake that ends it waits for a second poll that a restate keeps pushing out.
func TestRestatedShedDoesNotDeferItself(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	do(s, "s3cret", "p1", `{"desired":"off"}`) // restated before any poll
	if got := poll(s); got != control.RequestOff {
		t.Fatalf("desired = %v, want off", got)
	}
	if len(s.deferred) != 0 {
		t.Fatalf("deferred = %v, want empty; the shed was parked behind itself", s.deferred)
	}

	do(s, "s3cret", "p1", `{"desired":"on"}`)
	if got := poll(s); got != control.RequestOn {
		t.Errorf("desired = %v, want on; the applied shed still held the wake back", got)
	}
}

// The whole deferral rests on one invariant: a load has a deferred request only while its
// standing one is a shed no poll has seen. Promotion is what maintains it, and leaving the
// old slot behind would let a stale request surface after the next shed.
func TestPromotionLeavesNoDeferredRequestBehind(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	do(s, "s3cret", "p1", `{"desired":"on"}`)
	if len(s.deferred) != 1 {
		t.Fatalf("deferred = %v, want the wake parked behind the unapplied shed", s.deferred)
	}

	poll(s) // serves the shed and promotes
	if len(s.deferred) != 0 {
		t.Errorf("deferred = %v, want it cleared once promoted", s.deferred)
	}
	if e := s.want["p1"]; e.req != control.RequestOn || e.applied {
		t.Errorf("want[p1] = %+v, want an unapplied on", e)
	}
}

// The last request before the poll is the one that takes over: a deferred slot holds one
// request, not a queue to replay.
func TestOnlyTheLatestDeferredRequestSurvives(t *testing.T) {
	s := testServer(t)
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	do(s, "s3cret", "p1", `{"desired":"on"}`)
	do(s, "s3cret", "p1", `{"desired":"hold"}`)

	if got := poll(s); got != control.RequestOff {
		t.Fatalf("desired = %v, want off", got)
	}
	if got := poll(s); got != control.RequestHold {
		t.Errorf("desired = %v, want hold", got)
	}
}

// An expiring request takes its deferred successor with it, so a load nobody asks about
// again leaves nothing behind. Asserted on the map itself: Desired only ever reads want, so
// an orphan in deferred is invisible from the outside and a black-box check would pass
// whether or not the cleanup happens.
func TestExpiryClearsTheDeferredRequest(t *testing.T) {
	s := testServer(t)
	base := time.Now()
	s.now = func() time.Time { return base }
	do(s, "s3cret", "p1", `{"desired":"off"}`)
	do(s, "s3cret", "p1", `{"desired":"on"}`)

	s.now = func() time.Time { return base.Add(time.Hour) }
	if got, ok := s.Desired()["p1"]; ok {
		t.Fatalf("request survived its TTL: %v", got)
	}
	if len(s.deferred) != 0 {
		t.Errorf("deferred = %v, want it dropped with the request it was queued behind", s.deferred)
	}
}

// Deferring keeps the shed's clock alive: the deferred slot only refreshes itself, so a
// caller restating into it while the shed waits would watch that shed age out.
func TestDeferringRefreshesTheShedItWaitsOn(t *testing.T) {
	s := testServer(t)
	base := time.Now()
	s.now = func() time.Time { return base }
	do(s, "s3cret", "p1", `{"desired":"off"}`)

	s.now = func() time.Time { return base.Add(59 * time.Minute) }
	do(s, "s3cret", "p1", `{"desired":"on"}`)

	s.now = func() time.Time { return base.Add(61 * time.Minute) }
	if got, ok := s.Desired()["p1"]; !ok || got != control.RequestOff {
		t.Fatalf("desired = %v (present %v), want off; the shed expired under its deferred wake", got, ok)
	}
	s.Applied()
	if got := poll(s); got != control.RequestOn {
		t.Errorf("desired = %v, want on", got)
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
