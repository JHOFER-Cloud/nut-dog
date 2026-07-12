package effects

import (
	"fmt"
	"strings"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/JHOFER-Cloud/nut-dog/internal/nut"
)

// Shed-signal states written to a server's dummy-ups. "OB LB FSD" (on battery +
// low battery + forced-shutdown) is the unambiguous critical condition a
// monitoring upsmon secondary self-shuts on; "OL" (online) is the healthy state
// it stays up under. The exact string is validated by the dummy-ups integration
// test (test/integration/shed_test.sh).
const (
	statusShed = "OB LB FSD"
	statusOK   = "OL"
)

// VarSetter sets a NUT variable on a UPS. Abstracted so NUTShedder is testable
// without a live upsd.
type VarSetter func(ups, name, value string) error

// NUTShedder drives each NUT server's shed signal by setting ups.status on its
// dedicated dummy-ups served by nut-dog's local upsd. Asserting holds the signal
// critical (the server self-shuts); releasing returns it to OL so a rebooted
// server stays up.
type NUTShedder struct {
	// ShedUps maps a load name to its dummy-ups name (e.g. "p1" -> "shed-p1").
	ShedUps map[string]string
	Set     VarSetter
}

func (s NUTShedder) Assert(load string) error  { return s.set(load, statusShed) }
func (s NUTShedder) Release(load string) error { return s.set(load, statusOK) }

// ParseShedStatus maps a shed dummy-ups's ups.status back to a control.ShedState
// — the inverse of Assert/Release, so the controller can read the signal it drove
// and stay edge-triggered. FSD (forced shutdown, part of statusShed) marks the
// asserted signal; a plain OL marks released; anything else is unknown.
func ParseShedStatus(status string) control.ShedState {
	ol := false
	for f := range strings.FieldsSeq(status) {
		switch f {
		case "FSD":
			return control.ShedAsserted
		case "OL":
			ol = true
		}
	}
	if ol {
		return control.ShedReleased
	}
	return control.ShedUnknown
}

func (s NUTShedder) set(load, status string) error {
	ups, ok := s.ShedUps[load]
	if !ok {
		return fmt.Errorf("no shed ups configured for load %q", load)
	}
	return s.Set(ups, "ups.status", status)
}

// LocalVarSetter returns a VarSetter that writes to nut-dog's own upsd over the
// loopback NUT socket, authenticated as the admin user with SET rights.
func LocalVarSetter(addr string, opts nut.Options, timeout time.Duration) VarSetter {
	return func(ups, name, value string) error {
		return nut.SetVar(addr, ups, name, value, opts, timeout)
	}
}
