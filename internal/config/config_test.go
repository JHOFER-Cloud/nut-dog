package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

const sample = `
pollInterval: 15s
dryRun: true
localUpsd:
  listen: "0.0.0.0:3493"
  adminUser: nutdog
  adminPasswordEnv: NUTDOG_UPSD_PASSWORD
upses:
  ups-a:
    host: localhost:3493
    upsName: ups-a
    shedRuntime: 5m
    shedChargePct: 0
    recoverChargePct: 5
  ups-b:
    host: ups-b.hla1.jhofer.lan:3493
    upsName: nut
    tls: true
    insecure: true
    usernameEnv: UPSB_USERNAME
    passwordEnv: UPSB_PASSWORD
    shedRuntime: 5m
    recoverChargePct: 5
loads:
  bc1:
    type: chassis
    governedBy: [ups-a]
    cmc: { host: cmc.mgmt.hla1.jhofer.lan, sshUser: root }
  p1:
    type: nut-server
    governedBy: [ups-a, ups-b]
    wake:  { mac: "98:b7:85:20:77:6b", broadcast: "10.1.1.255:9" }
    probe: { host: pve-1.hla1.jhofer.lan:22 }
    secondary: { user: p1, passwordEnv: P1_SECONDARY_PW }
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if time.Duration(c.PollInterval) != 15*time.Second {
		t.Errorf("pollInterval = %v", time.Duration(c.PollInterval))
	}
	if !c.DryRun {
		t.Error("dryRun should be true")
	}
	if c.UPSes["ups-b"].UPSName != "nut" || !c.UPSes["ups-b"].TLS {
		t.Errorf("ups-b spec wrong: %+v", c.UPSes["ups-b"])
	}

	ups := c.ControlUPS()
	if ups["ups-a"].ShedRuntime != 300 {
		t.Errorf("ups-a shedRuntime = %d, want 300", ups["ups-a"].ShedRuntime)
	}
	if ups["ups-a"].RecoverCharge != 5 {
		t.Errorf("ups-a recoverCharge = %d, want 5", ups["ups-a"].RecoverCharge)
	}

	loads := c.ControlLoads()
	if loads["bc1"].Type != control.Chassis {
		t.Errorf("bc1 type = %v, want Chassis", loads["bc1"].Type)
	}
	if loads["p1"].Type != control.NutServer || len(loads["p1"].GovernedBy) != 2 {
		t.Errorf("p1 load wrong: %+v", loads["p1"])
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"zero poll", "pollInterval: 0s\nupses:\n  a: {host: h:1, upsName: a, shedRuntime: 5m}\n"},
		{
			"unknown governedBy",
			"pollInterval: 15s\nupses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  bc1: {type: chassis, governedBy: [nope], cmc: {host: c}}\n",
		},
		{
			"chassis without cmc",
			"pollInterval: 15s\nupses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  bc1: {type: chassis, governedBy: [ups-a]}\n",
		},
		{
			"unknown load type",
			"pollInterval: 15s\nupses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  x: {type: bogus, governedBy: [ups-a]}\n",
		},
		{
			"nut-server without wake",
			"pollInterval: 15s\nupses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  p1: {type: nut-server, governedBy: [ups-a], probe: {host: h:22}}\n",
		},
		{
			"powerAPI granting authority over a load that doesn't exist",
			"pollInterval: 15s\npowerAPI: {listen: \":9335\", tokenEnv: T, loads: [nope]}\n" +
				"upses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  bc1: {type: chassis, governedBy: [ups-a], cmc: {host: c}}\n",
		},
		{
			"negative startupGrace",
			"pollInterval: 15s\nstartupGrace: -5s\nupses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  bc1: {type: chassis, governedBy: [ups-a], cmc: {host: c}}\n",
		},
		{
			"powerAPI without a token env",
			"pollInterval: 15s\npowerAPI: {listen: \":9335\"}\nupses:\n  ups-a: {host: h:1, upsName: a, shedRuntime: 5m}\n" +
				"loads:\n  bc1: {type: chassis, governedBy: [ups-a], cmc: {host: c}}\n",
		},
	}
	for _, tt := range tests {
		if _, err := Load(writeConfig(t, tt.body)); err == nil {
			t.Errorf("%s: expected error, got nil", tt.name)
		}
	}
}
