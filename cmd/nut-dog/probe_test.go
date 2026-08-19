package main

import (
	"net"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

// A refusal means the host answered with an RST, so it is not down. energy-watchdog reads
// ActualDown as corroboration of power loss, which a restarting pveproxy would then supply
// falsely.
func TestProbeTreatsARefusalAsNotDown(t *testing.T) {
	// Bind then release: a port nothing listens on, on a host that does answer.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	// Exact value, not merely "not down": reporting a refusal as up is its own hazard, since
	// energy-watchdog never believes a node is gone while this reads up.
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
