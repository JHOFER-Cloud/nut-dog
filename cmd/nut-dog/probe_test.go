package main

import (
	"net"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

// A refused connection is proof the host is up: something had to be alive to send the RST.
// Reporting it as down was a lie the rest of the system leans on - energy-watchdog takes an
// ActualDown as corroboration that a host it cannot see has genuinely lost power, so a
// restarting pveproxy would have supplied that corroboration for a running machine.
func TestProbeTreatsARefusalAsNotDown(t *testing.T) {
	// Bind and immediately release, so the port is one nothing listens on but the host does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	// Asserting the exact value, not merely "not down": reporting a refusal as *up* is its own
	// hazard, since energy-watchdog never believes a node is gone while this says up - one
	// pveproxy restart would then pin p1 in that hold for as long as it lasted.
	if got := tcpProbe(addr); got != control.ActualUnknown {
		t.Errorf("probe of a refusing host = %v, want unknown: it answered, so it is not down,"+
			" but a shut port is no evidence that it is up either", got)
	}
}

func TestProbeReportsAListenerAsUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if got := tcpProbe(ln.Addr().String()); got != control.ActualUp {
		t.Errorf("probe = %v, want up", got)
	}
}
