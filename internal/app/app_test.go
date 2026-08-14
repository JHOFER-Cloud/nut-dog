package app

import (
	"io"
	"log/slog"
	"testing"
	"time"

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

// staticRequests is a fixed set of external power requests.
type staticRequests map[string]control.Request

func (s staticRequests) Desired() map[string]control.Request { return s }

func graceFixture(t *testing.T, actual control.ActualState) (*Controller, *recordApplier) {
	t.Helper()
	upsCfg := map[string]control.UPSConfig{"ups-a": {ShedRuntime: 300, RecoverCharge: 5}}
	loads := map[string]control.LoadConfig{"bc1": {Type: control.Chassis, GovernedBy: []string{"ups-a"}}}
	poller := mapPoller{"ups-a": {OK: true, Status: control.Status{OnLine: true}, Charge: 100}}
	applier := &recordApplier{}
	c := New(upsCfg, loads, poller, mapProber{"bc1": actual}, applier,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.StartupGrace = 90 * time.Second
	return c, applier
}

// A restart must not wake a load back up before the external controller has had a
// chance to say it wants it off.
func TestTickStartupGraceHoldsPowerOn(t *testing.T) {
	c, applier := graceFixture(t, control.ActualDown)
	start := time.Now()
	c.startedAt = start
	c.Now = func() time.Time { return start.Add(30 * time.Second) }

	c.Tick()
	if len(applier.got) != 0 {
		t.Errorf("power-on inside the grace: got %+v, want none", applier.got)
	}

	c.Now = func() time.Time { return start.Add(91 * time.Second) }
	c.Tick()
	want := control.Action{Kind: control.ChassisPowerUp, Load: "bc1"}
	if len(applier.got) != 1 || applier.got[0] != want {
		t.Errorf("after the grace: got %+v, want %+v", applier.got, want)
	}
}

// The grace must never delay an emergency: a restart during a power cut has to
// shed on the very first tick.
func TestTickStartupGraceNeverHoldsShed(t *testing.T) {
	c, applier := graceFixture(t, control.ActualUp)
	c.Poller = mapPoller{"ups-a": {OK: true, Status: control.Status{OnBattery: true}, Runtime: 120, Charge: 60}}
	start := time.Now()
	c.startedAt = start
	c.Now = func() time.Time { return start.Add(time.Second) }

	c.Tick()
	want := control.Action{Kind: control.ChassisShutdown, Load: "bc1"}
	if len(applier.got) != 1 || applier.got[0] != want {
		t.Errorf("got %+v, want %+v", applier.got, want)
	}
}

// An external request is honoured while the UPS is healthy, and overridden by it
// when it is not.
func TestTickExternalRequest(t *testing.T) {
	c, applier := graceFixture(t, control.ActualUp)
	c.StartupGrace = 0
	c.Requests = staticRequests{"bc1": control.RequestOff}

	c.Tick()
	want := control.Action{Kind: control.ChassisShutdown, Load: "bc1"}
	if len(applier.got) != 1 || applier.got[0] != want {
		t.Errorf("requested off: got %+v, want %+v", applier.got, want)
	}

	c2, applier2 := graceFixture(t, control.ActualDown)
	c2.StartupGrace = 0
	c2.Poller = mapPoller{"ups-a": {OK: false}} // unknown: never power on into it
	c2.Requests = staticRequests{"bc1": control.RequestOn}
	c2.Tick()
	if len(applier2.got) != 0 {
		t.Errorf("requested on with an unknown UPS: got %+v, want none", applier2.got)
	}
}
