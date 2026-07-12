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

// WakeInhibitor reports whether an external authority is deliberately holding a
// load powered off, so nut-dog should defer (skip the power-on) rather than fight
// it — e.g. energy-watchdog having shed a server for solar deficit. Optional: a
// nil WakeInhibitor never inhibits, and only loads it knows about are gated.
type WakeInhibitor interface {
	// InhibitWake reports whether the named load's power-on should be suppressed
	// this tick, with a short reason for the log.
	InhibitWake(load string) (inhibited bool, reason string)
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

	// WakeInhibitor lets an external authority veto a power-on so nut-dog defers to
	// it instead of fighting it. Optional: nil never inhibits.
	WakeInhibitor WakeInhibitor

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
	c.record(tel, actual, shed)

	actions := c.gateWakes(control.Decide(c.UPSConfigs, tel, c.Loads, actual, shed))
	if len(actions) > 0 {
		c.Log.Info("reconcile", "actions", len(actions))
	}
	c.Applier.Apply(actions)
	c.Metrics.ObserveReconcile()
}

// gateWakes drops power-on actions for loads an external authority is deliberately
// holding off (e.g. energy-watchdog powering a server down for solar deficit), so
// nut-dog defers instead of fighting the other controller. Every other action —
// including releasing nut-dog's own shed signal — passes through untouched, so
// the reconcile still converges everything it does own.
func (c *Controller) gateWakes(actions []control.Action) []control.Action {
	if c.WakeInhibitor == nil {
		return actions
	}
	// In-place filter: Decide returns a freshly allocated slice each tick, so
	// reusing its backing array is safe and keeps the no-drop path allocation-free.
	kept := actions[:0]
	for _, a := range actions {
		if isPowerOn(a.Kind) {
			if inhibited, reason := c.WakeInhibitor.InhibitWake(a.Load); inhibited {
				c.Log.Info("wake inhibited", "load", a.Load, "action", a.Kind.String(), "reason", reason)
				c.Metrics.RecordWakeInhibited(a.Load)
				continue
			}
		}
		kept = append(kept, a)
	}
	return kept
}

// isPowerOn is true for the actions that bring a load up — the ones a wake
// inhibitor gates.
func isPowerOn(k control.ActionKind) bool {
	return k == control.WakeServer || k == control.ChassisPowerUp
}

// record publishes the controller's interpretation for this tick: each UPS's
// classification and each load's desired/actual/shed state. It reuses the same
// pure functions Decide does, so the metrics can't drift from the decision.
func (c *Controller) record(tel map[string]control.Telemetry, actual map[string]control.ActualState, shed map[string]control.ShedState) {
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
		c.Metrics.RecordLoad(l, control.DesiredForLoad(states), actual[l], shed[l], lc.Type == control.NutServer)
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
