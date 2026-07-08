// Package effects executes the actions the control core decides. It is the only
// layer that touches the outside world (CMC over SSH, NUT shed signals, WoL), so
// it is where dryRun and loud-failure handling live. The core stays pure; the
// executor dispatches each action to an injectable implementation, which keeps
// the risky I/O behind interfaces we can fake in tests.
package effects

import (
	"fmt"
	"log/slog"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/JHOFER-Cloud/nut-dog/internal/metrics"
)

// Chassis commands a blade chassis through its CMC (racadm over SSH).
type Chassis interface {
	Shutdown(cmcHost string) error
	PowerUp(cmcHost string) error
}

// Shedder drives a NUT-capable server's shed signal (nut-dog's per-server
// dummy-ups), which the server's own upsmon self-shuts on / clears against.
type Shedder interface {
	Assert(server string) error
	Release(server string) error
}

// Waker sends a Wake-on-LAN magic packet.
type Waker interface {
	Wake(mac, broadcast string) error
}

// Target holds the per-load runtime details an action needs to act on.
type Target struct {
	CMCHost   string // chassis
	WakeMAC   string // nut-server
	WakeBcast string // nut-server (host:port)
}

// Executor applies control actions. Implementations may be nil for concerns a
// deployment doesn't use; only the kinds actually emitted are dispatched.
type Executor struct {
	DryRun  bool
	Targets map[string]Target
	Chassis Chassis
	Shedder Shedder
	Waker   Waker
	Metrics *metrics.Metrics // optional; nil-safe
	Log     *slog.Logger
}

// Apply runs every action, counting it (even in dryRun, so intended actions are
// visible) and logging (never swallowing) any failure so a broken CMC/WoL is
// visible rather than silent.
func (e *Executor) Apply(actions []control.Action) {
	for _, a := range actions {
		err := e.do(a)
		e.Metrics.RecordAction(a.Load, a.Kind.String(), err != nil)
		if err != nil {
			e.Log.Error("effect failed", "action", a.Kind.String(), "load", a.Load, "err", err)
		}
	}
}

// do dispatches one action. In dryRun it logs the intent and performs nothing.
// It fails loudly rather than acting on an empty target or a nil implementation
// (a mis-wiring), which is safer than silently shutting down the wrong thing.
func (e *Executor) do(a control.Action) error {
	if e.DryRun {
		e.Log.Info("dry-run: would apply", "action", a.Kind.String(), "load", a.Load)
		return nil
	}
	t, ok := e.Targets[a.Load]
	if !ok {
		return fmt.Errorf("no target configured for load %q", a.Load)
	}
	switch a.Kind {
	case control.ChassisShutdown, control.ChassisPowerUp:
		if e.Chassis == nil {
			return fmt.Errorf("chassis effect not configured for %q", a.Load)
		}
		if a.Kind == control.ChassisShutdown {
			return e.Chassis.Shutdown(t.CMCHost)
		}
		return e.Chassis.PowerUp(t.CMCHost)
	case control.AssertServerShed, control.ReleaseServerShed:
		if e.Shedder == nil {
			return fmt.Errorf("shedder effect not configured for %q", a.Load)
		}
		if a.Kind == control.AssertServerShed {
			return e.Shedder.Assert(a.Load)
		}
		return e.Shedder.Release(a.Load)
	case control.WakeServer:
		if e.Waker == nil {
			return fmt.Errorf("waker effect not configured for %q", a.Load)
		}
		return e.Waker.Wake(t.WakeMAC, t.WakeBcast)
	}
	return nil
}
