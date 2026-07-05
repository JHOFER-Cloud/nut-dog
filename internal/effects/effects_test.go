package effects

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

type fakeChassis struct {
	shutdown, powerup string
	err               error
}

func (f *fakeChassis) Shutdown(host string) error { f.shutdown = host; return f.err }
func (f *fakeChassis) PowerUp(host string) error  { f.powerup = host; return f.err }

type fakeShedder struct{ asserted, released string }

func (f *fakeShedder) Assert(s string) error  { f.asserted = s; return nil }
func (f *fakeShedder) Release(s string) error { f.released = s; return nil }

type fakeWaker struct{ mac, bcast string }

func (f *fakeWaker) Wake(mac, bcast string) error { f.mac, f.bcast = mac, bcast; return nil }

func newExecutor(dry bool) (*Executor, *fakeChassis, *fakeShedder, *fakeWaker) {
	c, s, w := &fakeChassis{}, &fakeShedder{}, &fakeWaker{}
	e := &Executor{
		DryRun: dry,
		Targets: map[string]Target{
			"bc1": {CMCHost: "cmc.host"},
			"p1":  {WakeMAC: "98:b7:85:20:77:6b", WakeBcast: "10.1.1.255:9"},
		},
		Chassis: c, Shedder: s, Waker: w,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return e, c, s, w
}

func TestApplyDispatch(t *testing.T) {
	e, c, s, w := newExecutor(false)
	e.Apply([]control.Action{
		{Kind: control.ChassisShutdown, Load: "bc1"},
		{Kind: control.ChassisPowerUp, Load: "bc1"},
		{Kind: control.AssertServerShed, Load: "p1"},
		{Kind: control.ReleaseServerShed, Load: "p1"},
		{Kind: control.WakeServer, Load: "p1"},
	})
	if c.shutdown != "cmc.host" || c.powerup != "cmc.host" {
		t.Errorf("chassis not dispatched with CMC host: %+v", c)
	}
	if s.asserted != "p1" || s.released != "p1" {
		t.Errorf("shedder not dispatched: %+v", s)
	}
	if w.mac != "98:b7:85:20:77:6b" || w.bcast != "10.1.1.255:9" {
		t.Errorf("waker not dispatched with target: %+v", w)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	e, c, s, w := newExecutor(true)
	e.Apply([]control.Action{
		{Kind: control.ChassisShutdown, Load: "bc1"},
		{Kind: control.WakeServer, Load: "p1"},
		{Kind: control.AssertServerShed, Load: "p1"},
	})
	if c.shutdown != "" || w.mac != "" || s.asserted != "" {
		t.Errorf("dry-run must not call any effect: chassis=%+v shed=%+v wake=%+v", c, s, w)
	}
}

func TestFailureIsLoggedNotSwallowed(t *testing.T) {
	var buf bytes.Buffer
	e, c, _, _ := newExecutor(false)
	e.Log = slog.New(slog.NewTextHandler(&buf, nil))
	c.err = errors.New("cmc unreachable")

	e.Apply([]control.Action{{Kind: control.ChassisShutdown, Load: "bc1"}})

	if !bytes.Contains(buf.Bytes(), []byte("effect failed")) ||
		!bytes.Contains(buf.Bytes(), []byte("cmc unreachable")) {
		t.Errorf("failure must be logged loudly, got: %s", buf.String())
	}
}
