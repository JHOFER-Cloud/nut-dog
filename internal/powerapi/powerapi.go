// Package powerapi serves the endpoint another controller (energy-watchdog) uses
// to ask for a load's power state. Requests are advisory: nut-dog folds them into
// its own decision, where a critical UPS still outranks them. A request is a level, not
// an event, and the latest one wins - except for a shed, which is always applied for at
// least one poll before anything supersedes it.
//
// A server's shutdown cannot be recalled once its upsmon has seen the signal, so what
// supersedes the shed only decides what happens next: "on" costs a full shutdown-and-WoL
// cycle, "hold" leaves the server down with its shed signal still asserted, since holding a
// load is by definition not moving it. Either beats the alternative, which is a shed that
// stops every guest and then never happens.
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

// entry is one load's standing request and when it was last restated. applied records
// that a reconcile has read it, which is what makes a shed undroppable; see deferred.
type entry struct {
	req     control.Request
	at      time.Time
	applied bool
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
	// deferred is the request that supersedes a shed no reconcile has read yet, parked
	// until that shed has been served for one poll. At most one per load - a later
	// request simply replaces it. Only a shed gets this treatment: a wake that never
	// reaches a poll costs a wake, a shed that never reaches one leaves the load up.
	deferred map[string]entry
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
		want: map[string]entry{}, deferred: map[string]entry{},
	}
}

// Desired implements app.PowerRequests, dropping requests nobody has restated inside the
// TTL. Note what expiry means: the load falls back to the UPS verdict, which on healthy
// sources is "on" - so a caller that dies eventually gets its load powered up, rather than
// pinned wherever it left it.
//
// Reading is repeatable: only the passage of time changes what comes back, so a second
// caller - a probe, a debug handler - cannot consume anything. Advancing the shed hold is
// Applied's job.
func (s *Server) Desired() map[string]control.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]control.Request, len(s.want))
	for load, e := range s.want {
		if s.ttl > 0 && s.now().Sub(e.at) >= s.ttl {
			delete(s.want, load)
			delete(s.deferred, load)
			s.log.Warn("power request expired; load follows its UPSes again",
				"load", load, "request", e.req.String(), "ttl", s.ttl.String())
			continue
		}
		out[load] = e.req
	}
	return out
}

// Applied records that a reconcile has acted on what Desired returned, which is what a shed
// has to survive before anything may take it back. Note what it does and does not claim: the
// reconcile decided on the shed, not that the signal reached the server. An effect that then
// fails is caught where every other one is - a loud log and nut_dog_action_failures_total -
// rather than here, because gating this on the signal instead would strand the deferred
// request for as long as the assert kept failing.
func (s *Server) Applied() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for load, e := range s.want {
		if d, ok := s.deferred[load]; ok {
			s.want[load] = d
			delete(s.deferred, load)
			s.log.Warn("shed applied; deferred power request now in force",
				"load", load, "request", d.req.String())
			continue
		}
		if !e.applied {
			e.applied = true
			s.want[load] = e
		}
	}
}

// Handler returns the mux serving the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/loads/{load}/power", s.handlePower)
	return mux
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
	// A load with no entry reads as NoRequest, which is neither RequestOff nor any d, so
	// both tests below are already false for it.
	cur := s.want[load]
	// A shed no reconcile has read yet has to reach one before anything can take it back.
	// nut-dog polls on its own schedule, so a wake landing milliseconds later would
	// otherwise overwrite the shed in place and it would never happen at all - which is
	// how a solar shed came to stop every guest and leave p1 running.
	deferring := cur.req == control.RequestOff && !cur.applied && d != control.RequestOff
	var changed, dropped bool
	if deferring {
		prev, had := s.deferred[load]
		changed = !had || prev.req != d
		s.deferred[load] = entry{req: d, at: s.now()}
		// Restating into the deferred slot proves the caller is alive, so the shed it is
		// queued behind must not age out from under it.
		cur.at = s.now()
		s.want[load] = cur
	} else {
		changed = cur.req != d
		// Restating the same request must keep applied: the caller restates every tick, so
		// rebuilding the entry here would put a long-standing shed permanently back into
		// "not yet read" and defer the wake that ends it by another poll.
		s.want[load] = entry{req: d, at: s.now(), applied: cur.req == d && cur.applied}
		_, dropped = s.deferred[load]
		delete(s.deferred, load)
	}
	s.mu.Unlock()

	// Only transitions are logged: the caller restates its wish every tick, and
	// logging each one would bury the actual decisions.
	if changed || dropped {
		msg := "power request"
		switch {
		case deferring:
			msg = "power request deferred until the shed has been applied"
		case dropped:
			// The deferral was announced; say that it is off again, or the "now in force"
			// line it promised just never arrives.
			msg = "power request replaced the deferred one"
		}
		s.log.Warn(msg, "load", load, "desired", body.Desired, "reason", body.Reason)
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
