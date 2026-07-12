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

type mapShedReader map[string]control.ShedState

func (m mapShedReader) ReadShed(load string) control.ShedState { return m[load] }

type recordApplier struct{ got []control.Action }

func (r *recordApplier) Apply(a []control.Action) { r.got = append(r.got, a...) }

// inhibitLoads inhibits the wake for exactly the loads it contains.
type inhibitLoads map[string]bool

func (i inhibitLoads) InhibitWake(load string) (bool, string) {
	if i[load] {
		return true, "held"
	}
	return false, ""
}

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

// A recovered server that an external authority is still holding off: nut-dog
// releases its own shed signal (it has no UPS reason to hold p1) but the WakeServer
// is inhibited, so it defers the power-on instead of fighting the other controller.
func TestTickWakeInhibited(t *testing.T) {
	upsCfg := map[string]control.UPSConfig{
		"ups-a": {ShedRuntime: 300, RecoverCharge: 5},
		"ups-b": {ShedRuntime: 300, RecoverCharge: 5},
	}
	loads := map[string]control.LoadConfig{
		"p1": {Type: control.NutServer, GovernedBy: []string{"ups-a", "ups-b"}},
	}
	poller := mapPoller{
		"ups-a": {OK: true, Status: control.Status{OnLine: true}, Charge: 100},
		"ups-b": {OK: true, Status: control.Status{OnLine: true}, Charge: 100},
	}
	prober := mapProber{"p1": control.ActualDown}
	applier := &recordApplier{}

	c := New(upsCfg, loads, poller, prober, applier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.ShedReader = mapShedReader{"p1": control.ShedAsserted} // release still needed
	c.WakeInhibitor = inhibitLoads{"p1": true}
	c.Tick()

	want := []control.Action{{Kind: control.ReleaseServerShed, Load: "p1"}}
	if len(applier.got) != len(want) || applier.got[0] != want[0] {
		t.Errorf("got %+v, want %+v (wake must be dropped, release kept)", applier.got, want)
	}
}

// With the inhibitor not gating this load, the wake goes through as usual.
func TestTickWakeNotInhibited(t *testing.T) {
	upsCfg := map[string]control.UPSConfig{
		"ups-a": {ShedRuntime: 300, RecoverCharge: 5},
		"ups-b": {ShedRuntime: 300, RecoverCharge: 5},
	}
	loads := map[string]control.LoadConfig{
		"p1": {Type: control.NutServer, GovernedBy: []string{"ups-a", "ups-b"}},
	}
	poller := mapPoller{
		"ups-a": {OK: true, Status: control.Status{OnLine: true}, Charge: 100},
		"ups-b": {OK: true, Status: control.Status{OnLine: true}, Charge: 100},
	}
	prober := mapProber{"p1": control.ActualDown}
	applier := &recordApplier{}

	c := New(upsCfg, loads, poller, prober, applier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.ShedReader = mapShedReader{"p1": control.ShedReleased} // only a wake is due
	c.WakeInhibitor = inhibitLoads{}                         // knows about no loads
	c.Tick()

	want := []control.Action{{Kind: control.WakeServer, Load: "p1"}}
	if len(applier.got) != len(want) || applier.got[0] != want[0] {
		t.Errorf("got %+v, want %+v", applier.got, want)
	}
}

// A healthy nut-server whose shed signal is already released must be silent: with
// a ShedReader wired, the reconcile is edge-triggered and re-drives nothing.
func TestTickHealthyServerSilentWithShedReader(t *testing.T) {
	upsCfg := map[string]control.UPSConfig{
		"ups-a": {ShedRuntime: 300, RecoverCharge: 5},
		"ups-b": {ShedRuntime: 300, RecoverCharge: 5},
	}
	loads := map[string]control.LoadConfig{
		"p1": {Type: control.NutServer, GovernedBy: []string{"ups-a", "ups-b"}},
	}
	poller := mapPoller{
		"ups-a": {OK: true, Status: control.Status{OnLine: true}, Charge: 100},
		"ups-b": {OK: true, Status: control.Status{OnLine: true}, Charge: 100},
	}
	prober := mapProber{"p1": control.ActualUp}
	applier := &recordApplier{}

	c := New(upsCfg, loads, poller, prober, applier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.ShedReader = mapShedReader{"p1": control.ShedReleased}
	c.Tick()

	if len(applier.got) != 0 {
		t.Errorf("expected no actions for a settled healthy server, got %+v", applier.got)
	}
}
