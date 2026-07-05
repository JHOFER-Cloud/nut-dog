// Package nut reads UPS telemetry from a NUT server and turns it into the
// control.Telemetry the decision core consumes. nut-dog polls both UPSes:
// UPS-A through the local snmp-ups driver (ups-a@localhost, plaintext) and
// UPS-B from its own NUT server (TLS + login). Confirmed against the real
// CyberPower RMCARD, snmp-ups surfaces the RFC1628 UPS-MIB as
// battery.runtime (seconds) / battery.charge (percent) / ups.status.
package nut

import (
	"strconv"
	"strings"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

// TelemetryFromVars maps a NUT variable set to Telemetry. A reading is only OK
// when ups.status is present — proof the server actually answered. Absent
// runtime/charge become -1 rather than a misleading 0.
func TelemetryFromVars(vars map[string]string) control.Telemetry {
	t := control.Telemetry{Runtime: -1, Charge: -1}
	if st, ok := vars["ups.status"]; ok {
		t.Status = parseStatus(st)
		t.OK = true
	}
	if v, ok := vars["battery.runtime"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			t.Runtime = n
		}
	}
	if v, ok := vars["battery.charge"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			t.Charge = n
		}
	}
	return t
}

// ParseUPSC turns the key/value output of `upsc <ups>` into Telemetry. Kept for
// debugging / CLI parity; the live path uses Fetch, which returns the same
// variable map.
func ParseUPSC(raw string) control.Telemetry {
	vars := make(map[string]string)
	for line := range strings.SplitSeq(raw, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		vars[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return TelemetryFromVars(vars)
}

// parseStatus reads the space-separated ups.status flags (e.g. "OB DISCHRG LB").
// Only the three that drive shedding are kept; the rest are ignored.
func parseStatus(val string) control.Status {
	var s control.Status
	for tok := range strings.FieldsSeq(val) {
		switch tok {
		case "OL":
			s.OnLine = true
		case "OB":
			s.OnBattery = true
		case "LB":
			s.LowBattery = true
		}
	}
	return s
}
