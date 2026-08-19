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

	if got := tcpProbe(addr); got == control.ActualDown {
		t.Errorf("probe of a refusing host = down; the host answered, only the port is shut")
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
