// Package app assembles nut-dog's reconcile loop from injectable parts. One
// Tick reads every UPS's telemetry and every load's actual power state, runs the
// pure controller, and applies the resulting actions. Keeping the parts behind
// interfaces makes the loop testable without hardware; main wires the real
// implementations.
package app

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/JHOFER-Cloud/nut-dog/internal/metrics"
)

// Poller reads one UPS's current telemetry (never errors: a failed read is
// telemetry with OK=false, which the controller treats as Unknown).
type Poller interface {
	Poll(ups string) control.Telemetry
}

// Prober reads one load's actual power state.
type Prober interface {
	Probe(load string) control.ActualState
}

// ShedReader reads the current position of a NUT server's shed signal (the
// ups.status nut-dog serves for it on the local upsd), so the reconcile stays
// edge-triggered. Optional: when nil, shed state reads as Unknown and the
// controller falls back to re-driving the signal every tick.
type ShedReader interface {
	ReadShed(load string) control.ShedState
}

// Applier performs the decided actions.
type Applier interface {
	Apply(actions []control.Action)
}

// PowerRequests reports what an external controller (energy-watchdog) currently
// wants each load's power state to be. Absent loads mean no request.
type PowerRequests interface {
	Desired() map[string]control.Request
}

// Controller holds everything one reconcile Tick needs.
type Controller struct {
	UPSConfigs map[string]control.UPSConfig
	Loads      map[string]control.LoadConfig
	Poller     Poller
	Prober     Prober
	ShedReader ShedReader
	Applier    Applier
	Log        *slog.Logger

	// Requests is the external controller's wanted power state per load. Optional:
	// nil means every load follows its UPSes alone.
	Requests PowerRequests

	// StartupGrace suppresses power-on actions for this long after StartClock, so a
	// restart doesn't wake a load the external controller wants off before its
	// first request has arrived. Shed actions are never suppressed. 0 disables.
	StartupGrace time.Duration
	// Now is the clock, for tests. nil means time.Now.
	Now func() time.Time

	startedAt time.Time

	// Metrics records the controller's interpretation (source classification,
	// per-load desired/actual/shed) and the reconcile heartbeat each tick.
	// Optional: all recorders are no-ops on a nil *Metrics.
	Metrics *metrics.Metrics

	// Verbose logs the telemetry + actual state each tick (used in dryRun so
	// observe-mode is actually observable). Off when armed to keep logs quiet.
	Verbose bool

	upsNames  []string
	loadNames []string
}

// New builds a Controller and caches sorted names for deterministic iteration.
func New(
	upsConfigs map[string]control.UPSConfig,
	loads map[string]control.LoadConfig,
	poller Poller, prober Prober, applier Applier, log *slog.Logger,
) *Controller {
	c := &Controller{
		UPSConfigs: upsConfigs, Loads: loads,
		Poller: poller, Prober: prober, Applier: applier, Log: log,
	}
	for name := range upsConfigs {
		c.upsNames = append(c.upsNames, name)
	}
	for name := range loads {
		c.loadNames = append(c.loadNames, name)
	}
	sort.Strings(c.upsNames)
	sort.Strings(c.loadNames)
	return c
}

// StartClock starts the startup grace. Call it when the loop actually begins:
// anything blocking in between (preflight probes over SSH and the network) would
// otherwise burn the budget the grace exists to provide.
func (c *Controller) StartClock() { c.startedAt = c.now()() }

func (c *Controller) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

// Tick runs one reconcile pass.
func (c *Controller) Tick() {
	tel := make(map[string]control.Telemetry, len(c.upsNames))
	for _, u := range c.upsNames {
		tel[u] = c.Poller.Poll(u)
	}
	actual := make(map[string]control.ActualState, len(c.loadNames))
	shed := make(map[string]control.ShedState, len(c.loadNames))
	for _, l := range c.loadNames {
		actual[l] = c.Prober.Probe(l)
		// Shed signals exist only for NUT servers; reading it back is what keeps
		// the reconcile edge-triggered instead of re-driving OL every tick.
		if c.ShedReader != nil && c.Loads[l].Type == control.NutServer {
			shed[l] = c.ShedReader.ReadShed(l)
		}
	}
	if c.Verbose {
		for _, u := range c.upsNames {
			t := tel[u]
			c.Log.Info("ups", "name", u, "ok", t.OK,
				"status", statusString(t.Status), "runtime_s", t.Runtime, "charge_pct", t.Charge)
		}
		for _, l := range c.loadNames {
			if c.Loads[l].Type == control.NutServer {
				c.Log.Info("load", "name", l, "actual", actualString(actual[l]), "shed", shedString(shed[l]))
			} else {
				c.Log.Info("load", "name", l, "actual", actualString(actual[l]))
			}
		}
	}
	ext := c.requested()
	c.record(tel, actual, shed, ext)

	actions := c.holdStartupWakes(control.Decide(c.UPSConfigs, tel, c.Loads, actual, shed, ext))
	if len(actions) > 0 {
		c.Log.Info("reconcile", "actions", len(actions))
	}
	c.Applier.Apply(actions)
	c.Metrics.ObserveReconcile()
}

// holdStartupWakes drops power-on actions during the startup grace. An external
// controller's request arrives on its own schedule, so acting on "no request"
// straight after a restart can wake a load it wants off, just to shed it again.
// Sheds always pass: a restart must never delay an emergency.
func (c *Controller) holdStartupWakes(actions []control.Action) []control.Action {
	if c.StartupGrace <= 0 {
		return actions
	}
	now := c.now()
	if c.startedAt.IsZero() {
		c.startedAt = now()
	}
	if now().Sub(c.startedAt) >= c.StartupGrace {
		c.Metrics.RecordStartupGrace(false)
		return actions
	}
	c.Metrics.RecordStartupGrace(true)
	kept := actions[:0]
	for _, a := range actions {
		if isPowerOn(a.Kind) {
			c.Log.Info("power-on held during startup grace", "load", a.Load, "action", a.Kind.String())
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// isPowerOn is true for the actions that bring a load up.
func isPowerOn(k control.ActionKind) bool {
	return k == control.WakeServer || k == control.ChassisPowerUp
}

func (c *Controller) requested() map[string]control.Request {
	if c.Requests == nil {
		return nil
	}
	return c.Requests.Desired()
}

// record publishes the controller's interpretation for this tick: each UPS's
// classification and each load's desired/actual/shed state. It reuses the same
// pure functions Decide does, so the metrics can't drift from the decision.
func (c *Controller) record(tel map[string]control.Telemetry, actual map[string]control.ActualState, shed map[string]control.ShedState, ext map[string]control.Request) {
	if c.Metrics == nil {
		return
	}
	src := make(map[string]control.SourceState, len(c.upsNames))
	for _, u := range c.upsNames {
		src[u] = control.Classify(c.UPSConfigs[u], tel[u])
		c.Metrics.RecordSource(u, src[u])
	}
	for _, l := range c.loadNames {
		lc := c.Loads[l]
		states := make([]control.SourceState, 0, len(lc.GovernedBy))
		for _, u := range lc.GovernedBy {
			states = append(states, src[u])
		}
		c.Metrics.RecordLoad(l, control.DesiredForLoad(states, ext[l]), ext[l], actual[l], shed[l], lc.Type == control.NutServer)
	}
}

func statusString(s control.Status) string {
	var parts []string
	if s.OnLine {
		parts = append(parts, "OL")
	}
	if s.OnBattery {
		parts = append(parts, "OB")
	}
	if s.LowBattery {
		parts = append(parts, "LB")
	}
	if len(parts) == 0 {
		return "?"
	}
	return strings.Join(parts, " ")
}

func actualString(a control.ActualState) string {
	switch a {
	case control.ActualUp:
		return "up"
	case control.ActualDown:
		return "down"
	default:
		return "unknown"
	}
}

func shedString(s control.ShedState) string {
	switch s {
	case control.ShedAsserted:
		return "asserted"
	case control.ShedReleased:
		return "released"
	default:
		return "unknown"
	}
}
