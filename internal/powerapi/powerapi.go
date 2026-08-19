// Package powerapi serves the endpoint another controller (energy-watchdog) uses
// to ask for a load's power state. Requests are advisory: nut-dog folds them into
// its own decision, where a critical UPS still outranks them.
package powerapi

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

// requestByName is the wire vocabulary. "hold" is an active request to leave the
// load alone, not a withdrawal: it stops nut-dog powering the load on by itself
// while the caller is mid-operation.
var requestByName = map[string]control.Request{
	"on":   control.RequestOn,
	"off":  control.RequestOff,
	"hold": control.RequestHold,
}

// entry is one load's standing request and when it was last restated.
type entry struct {
	req control.Request
	at  time.Time
}

// observation is the last probed power state for a load and when it was taken. Served so an
// external controller can read nut-dog's direct view instead of inferring power from a source
// with its own failure modes - Proxmox reports a node partitioned from its cluster as offline
// whether or not the host is running.
type observation struct {
	state control.ActualState
	at    time.Time
}

// Server holds the last request per load and serves the API. State is in memory
// only: callers restate their wish every tick, so nothing here has to survive a
// restart, and no stale file can hold a load down.
type Server struct {
	token string
	loads map[string]bool
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger

	mu   sync.Mutex
	want map[string]entry
	seen map[string]observation
}

// New builds a Server accepting requests for the named loads. A request older than
// ttl is dropped, so a caller that dies cannot pin a load forever; 0 never expires.
func New(token string, loads []string, ttl time.Duration, log *slog.Logger) *Server {
	known := make(map[string]bool, len(loads))
	for _, l := range loads {
		known[l] = true
	}
	return &Server{
		token: token, loads: known, ttl: ttl, now: time.Now, log: log,
		want: map[string]entry{}, seen: map[string]observation{},
	}
}

// PublishActual records one reconcile tick's probe results, implementing app.ActualSink.
// Entries do not expire: the age is served alongside the state so the caller applies its own
// freshness bound rather than this end silently downgrading a stale reading to unknown.
func (s *Server) PublishActual(actual map[string]control.ActualState, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for load, st := range actual {
		s.seen[load] = observation{state: st, at: at}
	}
}

// Desired implements app.PowerRequests, dropping requests nobody has restated inside the
// TTL. Note what expiry means: the load falls back to the UPS verdict, which on healthy
// sources is "on" - so a caller that dies eventually gets its load powered up, rather than
// pinned wherever it left it.
func (s *Server) Desired() map[string]control.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]control.Request, len(s.want))
	for load, e := range s.want {
		if s.ttl > 0 && s.now().Sub(e.at) >= s.ttl {
			delete(s.want, load)
			s.log.Warn("power request expired; load follows its UPSes again",
				"load", load, "request", e.req.String(), "ttl", s.ttl.String())
			continue
		}
		out[load] = e.req
	}
	return out
}

// Handler returns the mux serving the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/loads/{load}/power", s.handlePower)
	mux.HandleFunc("GET /api/loads/{load}/state", s.handleState)
	return mux
}

// handleState serves the load's last probed power state. ageSeconds is computed here rather
// than shipping a timestamp, so freshness does not depend on the caller's clock agreeing with
// ours.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		s.log.Warn("state request rejected: bad token", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	load := r.PathValue("load")
	if !s.loads[load] {
		http.Error(w, "unknown load", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	o, ok := s.seen[load]
	s.mu.Unlock()

	body := struct {
		Actual     string `json:"actual"`
		AgeSeconds int    `json:"ageSeconds"`
	}{Actual: control.ActualUnknown.String()}
	if ok {
		// Before the first tick the zero value stands, which is unknown - no opinion.
		body.Actual = o.state.String()
		body.AgeSeconds = int(s.now().Sub(o.at).Seconds())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		s.log.Warn("power request rejected: bad token", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	load := r.PathValue("load")
	if !s.loads[load] {
		http.Error(w, "unknown load", http.StatusNotFound)
		return
	}
	var body struct {
		Desired string `json:"desired"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, `want {"desired": "on"|"off"|"hold"}`, http.StatusBadRequest)
		return
	}
	d, ok := requestByName[body.Desired]
	if !ok {
		http.Error(w, `want {"desired": "on"|"off"|"hold"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	changed := s.want[load].req != d
	s.want[load] = entry{req: d, at: s.now()}
	s.mu.Unlock()

	// Only transitions are logged: the caller restates its wish every tick, and
	// logging each one would bury the actual decisions.
	if changed {
		s.log.Warn("power request", "load", load, "desired", body.Desired, "reason", body.Reason)
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorised checks the bearer token. The scheme is matched case-insensitively, as RFC 7235
// requires. The comparison is constant-time in the token's content; its length still leaks,
// which is not worth defending for a shared secret.
func (s *Server) authorised(r *http.Request) bool {
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}
