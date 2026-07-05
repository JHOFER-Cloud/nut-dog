// Package app assembles nut-dog's reconcile loop from injectable parts. One
// Tick reads every UPS's telemetry and every load's actual power state, runs the
// pure controller, and applies the resulting actions. Keeping the parts behind
// interfaces makes the loop testable without hardware; main wires the real
// implementations.
package app

import (
	"log/slog"
	"sort"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
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

// Applier performs the decided actions.
type Applier interface {
	Apply(actions []control.Action)
}

// Controller holds everything one reconcile Tick needs.
type Controller struct {
	UPSConfigs map[string]control.UPSConfig
	Loads      map[string]control.LoadConfig
	Poller     Poller
	Prober     Prober
	Applier    Applier
	Log        *slog.Logger

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
	for _, l := range c.loadNames {
		actual[l] = c.Prober.Probe(l)
	}
	actions := control.Decide(c.UPSConfigs, tel, c.Loads, actual)
	if len(actions) > 0 {
		c.Log.Info("reconcile", "actions", len(actions))
	}
	c.Applier.Apply(actions)
}
