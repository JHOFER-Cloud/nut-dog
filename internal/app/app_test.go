package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

type mapPoller map[string]control.Telemetry

func (m mapPoller) Poll(ups string) control.Telemetry { return m[ups] }

type mapProber map[string]control.ActualState

func (m mapProber) Probe(load string) control.ActualState { return m[load] }

type recordApplier struct{ got []control.Action }

func (r *recordApplier) Apply(a []control.Action) { r.got = append(r.got, a...) }

// A full grid-loss tick: UPS-A critical, BC1 (chassis) up, p1 up -> shed both.
func TestTickGridLoss(t *testing.T) {
	upsCfg := map[string]control.UPSConfig{
		"ups-a": {ShedRuntime: 300, RecoverCharge: 5},
		"ups-b": {ShedRuntime: 300, RecoverCharge: 5},
	}
	loads := map[string]control.LoadConfig{
		"bc1": {Type: control.Chassis, GovernedBy: []string{"ups-a"}},
		"p1":  {Type: control.NutServer, GovernedBy: []string{"ups-a", "ups-b"}},
	}
	poller := mapPoller{
		"ups-a": {OK: true, Status: control.Status{OnBattery: true}, Runtime: 120, Charge: 60},
		"ups-b": {OK: true, Status: control.Status{OnLine: true}, Charge: 95},
	}
	prober := mapProber{"bc1": control.ActualUp, "p1": control.ActualUp}
	applier := &recordApplier{}

	c := New(upsCfg, loads, poller, prober, applier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Tick()

	want := []control.Action{
		{Kind: control.ChassisShutdown, Load: "bc1"},
		{Kind: control.AssertServerShed, Load: "p1"},
	}
	if len(applier.got) != len(want) || applier.got[0] != want[0] || applier.got[1] != want[1] {
		t.Errorf("got %+v, want %+v", applier.got, want)
	}
}

// All healthy, nothing shed -> no actions.
func TestTickAllHealthyNoActions(t *testing.T) {
	upsCfg := map[string]control.UPSConfig{"ups-a": {ShedRuntime: 300, RecoverCharge: 5}}
	loads := map[string]control.LoadConfig{"bc1": {Type: control.Chassis, GovernedBy: []string{"ups-a"}}}
	poller := mapPoller{"ups-a": {OK: true, Status: control.Status{OnLine: true}, Charge: 100}}
	prober := mapProber{"bc1": control.ActualUp}
	applier := &recordApplier{}

	New(upsCfg, loads, poller, prober, applier, slog.New(slog.NewTextHandler(io.Discard, nil))).Tick()

	if len(applier.got) != 0 {
		t.Errorf("expected no actions, got %+v", applier.got)
	}
}
