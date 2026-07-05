// Package control is nut-dog's pure decision core. It is stateless and
// level-triggered: on every poll the caller reads live telemetry from each UPS
// and the actual power state of each load, hands them to Decide, and gets back
// the actions to reconcile reality toward what the telemetry implies. There are
// no timers and no persisted state, so a pod restart simply re-reads the world
// and continues — nothing to lose or re-arm.
//
// Roles:
//   - UPS-A (CyberPower RMCARD) is the grid-loss sensor and powers the blade
//     chassis BC1. UPS-B (UniFi) powers the networking tier plus the server p1.
//   - A load is shed when ANY governing UPS is critical and only powered back
//     when ALL of them are healthy again, with a charge deadband in between so
//     it never flaps.
//   - A chassis (BC1) is commanded directly via racadm. A NUT-capable server
//     (p1, and any future one) is shed over NUT — nut-dog drives a shed signal
//     its upsmon self-shuts on — and woken via Wake-on-LAN. Adding a server is
//     then config only, no code change.
package control

import (
	"sort"
	"strconv"
)

// Status holds the NUT ups.status flags we act on, already parsed from the
// raw string (e.g. "OB DISCHRG LB").
type Status struct {
	OnLine     bool // OL
	OnBattery  bool // OB
	LowBattery bool // LB — the UPS itself declaring critical
}

// Telemetry is a single UPS reading. OK is false when the poll failed
// (unreachable / unparseable); Runtime and Charge are -1 when absent.
type Telemetry struct {
	OK      bool
	Status  Status
	Runtime int // battery.runtime, seconds
	Charge  int // battery.charge, percent
}

// UPSConfig are the per-UPS thresholds, all sourced from the fleet ConfigMap.
type UPSConfig struct {
	ShedRuntime   int // shed when on battery AND runtime <= this (seconds)
	ShedCharge    int // ...or charge <= this (percent); 0 disables the charge trigger
	RecoverCharge int // only recover when online AND charge >= this (percent)
}

// SourceState is a UPS classified against its thresholds.
type SourceState int

const (
	SourceUnknown SourceState = iota // no data this poll — fail safe, never sheds on this alone
	SourceShed                       // critical: shed loads it governs
	SourceHold                       // in the deadband — neither shed nor recover
	SourceHealthy                    // online and recharged past the recover floor
)

// Classify maps one UPS reading to a SourceState. On battery it sheds on the
// UPS's own LB flag, on a runtime floor, or (optionally) a charge floor; online
// it is healthy once charge has climbed back to the recover floor, else it holds
// (recharging). Anything indeterminate holds — we only act on clear signals.
func Classify(c UPSConfig, t Telemetry) SourceState {
	if !t.OK {
		return SourceUnknown
	}
	switch {
	case t.Status.OnBattery:
		if t.Status.LowBattery {
			return SourceShed
		}
		if t.Runtime >= 0 && t.Runtime <= c.ShedRuntime {
			return SourceShed
		}
		if c.ShedCharge > 0 && t.Charge >= 0 && t.Charge <= c.ShedCharge {
			return SourceShed
		}
		return SourceHold
	case t.Status.OnLine:
		if t.Charge < 0 || t.Charge >= c.RecoverCharge {
			return SourceHealthy
		}
		return SourceHold
	default:
		return SourceHold
	}
}

// Desired is what a load's power state should be this poll.
type Desired int

const (
	DesiredHold Desired = iota // leave the load as it is
	DesiredOff                 // load should be shed
	DesiredOn                  // load should be running
)

// DesiredForLoad combines the states of a load's governing UPSes. A single
// critical source sheds it; if no source is shedding but any is unknown we hold
// (fail safe — never shed on missing data, never recover into uncertainty); it
// runs only when every source is healthy.
func DesiredForLoad(states []SourceState) Desired {
	anyShed, anyUnknown, allHealthy := false, false, true
	for _, s := range states {
		if s != SourceHealthy {
			allHealthy = false
		}
		switch s {
		case SourceShed:
			anyShed = true
		case SourceUnknown:
			anyUnknown = true
		}
	}
	switch {
	case anyShed:
		return DesiredOff
	case anyUnknown:
		return DesiredHold
	case allHealthy:
		return DesiredOn
	default:
		return DesiredHold
	}
}

// LoadType selects how a load is commanded.
type LoadType int

const (
	Chassis   LoadType = iota // dumb chassis: direct racadm shutdown/powerup
	NutServer                 // NUT-capable server: shed over NUT, wake via WoL
)

// ActualState is a load's observed power state (reachability probe).
type ActualState int

const (
	ActualUnknown ActualState = iota
	ActualUp
	ActualDown
)

// ActionKind is a concrete effect the executor performs.
type ActionKind int

const (
	ChassisShutdown   ActionKind = iota // racadm chassisaction graceshutdown
	ChassisPowerUp                      // racadm chassisaction powerup
	AssertServerShed                    // drive the server's NUT shed signal critical
	ReleaseServerShed                   // drive the server's NUT shed signal back to OK
	WakeServer                          // Wake-on-LAN
)

func (k ActionKind) String() string {
	switch k {
	case ChassisShutdown:
		return "ChassisShutdown"
	case ChassisPowerUp:
		return "ChassisPowerUp"
	case AssertServerShed:
		return "AssertServerShed"
	case ReleaseServerShed:
		return "ReleaseServerShed"
	case WakeServer:
		return "WakeServer"
	default:
		return "ActionKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Action is a single effect targeting a named load.
type Action struct {
	Kind ActionKind
	Load string
}

// ReconcileLoad turns a desired state + observed actual state into the actions
// needed to close the gap. It is idempotent: for a chassis it only commands a
// change when actual disagrees with desired; for a NUT server it continuously
// holds the shed signal in the right position (the executor applies that
// idempotently) and only WoLs while the server is desired-on but still down.
func ReconcileLoad(name string, lt LoadType, d Desired, actual ActualState) []Action {
	switch lt {
	case Chassis:
		switch d {
		case DesiredOff:
			if actual == ActualUp {
				return []Action{{ChassisShutdown, name}}
			}
		case DesiredOn:
			if actual == ActualDown {
				return []Action{{ChassisPowerUp, name}}
			}
		}
	case NutServer:
		switch d {
		case DesiredOff:
			// Hold the shed signal asserted; the server's own upsmon self-shuts.
			return []Action{{AssertServerShed, name}}
		case DesiredOn:
			acts := []Action{{ReleaseServerShed, name}}
			if actual == ActualDown {
				acts = append(acts, Action{WakeServer, name})
			}
			return acts
		}
	}
	return nil
}

// LoadConfig describes one controllable load.
type LoadConfig struct {
	Type       LoadType
	GovernedBy []string // UPS names; sheds if any is critical, runs when all healthy
}

// Decide is the whole per-poll transition: classify every UPS, then reconcile
// every load against its governing UPSes and its observed state. Pure and
// deterministic (loads are processed in name order), so the full policy is
// exhaustively unit-testable with plain tables.
func Decide(
	upsCfgs map[string]UPSConfig,
	tel map[string]Telemetry,
	loads map[string]LoadConfig,
	actual map[string]ActualState,
) []Action {
	src := make(map[string]SourceState, len(upsCfgs))
	for name, c := range upsCfgs {
		src[name] = Classify(c, tel[name])
	}

	names := make([]string, 0, len(loads))
	for name := range loads {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []Action
	for _, name := range names {
		lc := loads[name]
		states := make([]SourceState, 0, len(lc.GovernedBy))
		for _, u := range lc.GovernedBy {
			states = append(states, src[u])
		}
		out = append(out, ReconcileLoad(name, lc.Type, DesiredForLoad(states), actual[name])...)
	}
	return out
}
