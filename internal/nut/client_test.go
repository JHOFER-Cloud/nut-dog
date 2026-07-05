package nut

import (
	"bufio"
	"strings"
	"testing"
)

func TestCollectVars(t *testing.T) {
	resp := "BEGIN LIST VAR nut\n" +
		"VAR nut battery.charge \"100\"\n" +
		"VAR nut battery.runtime \"900\"\n" +
		"VAR nut ups.status \"OL CHRG\"\n" +
		"VAR nut ups.mfr \"UniFi\"\n" +
		"END LIST VAR nut\n"

	vars, err := collectVars(bufio.NewReader(strings.NewReader(resp)), "nut")
	if err != nil {
		t.Fatalf("collectVars: %v", err)
	}
	if vars["battery.charge"] != "100" || vars["battery.runtime"] != "900" {
		t.Errorf("bad numeric vars: %+v", vars)
	}
	// Quoted value with an embedded space must survive intact.
	if vars["ups.status"] != "OL CHRG" {
		t.Errorf("status = %q, want %q", vars["ups.status"], "OL CHRG")
	}

	// And it should classify correctly through the shared mapper.
	tel := TelemetryFromVars(vars)
	if !tel.OK || !tel.Status.OnLine || tel.Runtime != 900 || tel.Charge != 100 {
		t.Errorf("telemetry = %+v", tel)
	}
}

func TestCollectVarsError(t *testing.T) {
	_, err := collectVars(bufio.NewReader(strings.NewReader("ERR ACCESS-DENIED\n")), "nut")
	if err == nil || !strings.Contains(err.Error(), "ACCESS-DENIED") {
		t.Fatalf("want ACCESS-DENIED error, got %v", err)
	}
}

func TestParseVarLine(t *testing.T) {
	name, val, ok := parseVarLine(`VAR nut ups.status "OB DISCHRG LB"`, "nut")
	if !ok || name != "ups.status" || val != "OB DISCHRG LB" {
		t.Errorf("got name=%q val=%q ok=%v", name, val, ok)
	}
	// Wrong ups name is ignored.
	if _, _, ok := parseVarLine(`VAR other battery.charge "50"`, "nut"); ok {
		t.Errorf("expected mismatch to be rejected")
	}
}
