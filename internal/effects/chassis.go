package effects

import (
	"fmt"
	"strings"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

// Runner executes a single command on a host over SSH and returns its combined
// output. It is an interface so the racadm command building and output parsing
// can be tested without a real CMC.
type Runner interface {
	Run(host, cmd string) (string, error)
}

// RacadmChassis controls the M1000e blade chassis through its CMC via racadm
// over SSH. Shedding powers the whole chassis down (which cuts AC to the blades,
// so on power-up they self-boot via their iDRAC power-restore policy); recovery
// powers the chassis back up. This mirrors the graceful "power off" from the CMC
// UI. If a firmware powers the chassis down without gracefully stopping the
// blade OSes, prepend a `serveraction -a graceshutdown` step (the service key is
// provisioned with Server Administrator privilege for exactly that fallback).
type RacadmChassis struct {
	R Runner
}

func (c RacadmChassis) Shutdown(cmcHost string) error {
	return c.act(cmcHost, "chassisaction -m chassis powerdown")
}

func (c RacadmChassis) PowerUp(cmcHost string) error {
	return c.act(cmcHost, "chassisaction -m chassis powerup")
}

func (c RacadmChassis) act(host, sub string) error {
	out, err := c.R.Run(host, "racadm "+sub)
	if err != nil {
		return fmt.Errorf("cmc %q: %w (%s)", sub, err, strings.TrimSpace(out))
	}
	if racadmFailed(out) {
		return fmt.Errorf("cmc %q reported failure: %s", sub, strings.TrimSpace(out))
	}
	return nil
}

// PowerState reports whether the chassis is up, defined as "any populated blade
// is powered on". Used as BC1's actual-state probe in the reconcile loop.
func (c RacadmChassis) PowerState(cmcHost string) (control.ActualState, error) {
	out, err := c.R.Run(cmcHost, "racadm getmodinfo")
	if err != nil {
		return control.ActualUnknown, fmt.Errorf("cmc getmodinfo: %w", err)
	}
	return parseChassisPower(out), nil
}

// parseChassisPower scans getmodinfo output for Server rows. Columns are
// "<module> <presence> <pwrState> <health> <svcTag>"; a present, powered blade
// keys off presence=="Present" (note absent rows read "Not Present", so the
// second field is "Not") and pwrState=="ON".
func parseChassisPower(getmodinfo string) control.ActualState {
	sawServer, anyOn := false, false
	for line := range strings.SplitSeq(getmodinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[0], "Server-") {
			continue
		}
		sawServer = true
		if fields[1] == "Present" && strings.EqualFold(fields[2], "ON") {
			anyOn = true
		}
	}
	switch {
	case !sawServer:
		return control.ActualUnknown
	case anyOn:
		return control.ActualUp
	default:
		return control.ActualDown
	}
}

// racadmFailed detects a racadm error in output when the exit code alone did not
// surface it (some subcommands print ERROR but exit 0).
func racadmFailed(out string) bool {
	return strings.Contains(strings.ToUpper(out), "ERROR")
}
