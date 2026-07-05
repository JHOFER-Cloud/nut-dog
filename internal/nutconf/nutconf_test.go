package nutconf

import (
	"strings"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		LocalUpsd: config.LocalUpsdSpec{
			Listen: "0.0.0.0:3493", AdminUser: "nutdog", AdminPasswordEnv: "ADMIN_PW",
		},
		UPSes: map[string]config.UPSSpec{
			"ups-a": {
				Host: "localhost:3493", UPSName: "ups-a",
				Driver: &config.DriverSpec{
					Type: "snmp-ups", Port: "rmcard.host",
					Options:       map[string]string{"snmp_version": "v3", "secName": "snmp1"},
					SecretOptions: map[string]string{"authPassword": "SNMP_AUTH", "privPassword": "SNMP_PRIV"},
				},
			},
			"ups-b": {Host: "ups-b:3493", UPSName: "nut"}, // remote, no driver
		},
		Loads: map[string]config.LoadSpec{
			"p1": {Type: config.TypeNutServer, Secondary: &config.SecondarySpec{User: "p1", PasswordEnv: "P1_PW"}},
			"p2": {Type: config.TypeNutServer, Secondary: &config.SecondarySpec{User: "p2", PasswordEnv: "P2_PW"}},
		},
	}
}

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// The whole point: two servers (p1, p2) produce two shed signals + two logins,
// purely from config — nothing hardcoded to a single host.
func TestRenderGeneratesPerServer(t *testing.T) {
	secrets := map[string]string{
		"ADMIN_PW": "adminpw", "SNMP_AUTH": "authpw", "SNMP_PRIV": "privpw",
		"P1_PW": "p1pw", "P2_PW": "p2pw",
	}
	files, err := Render(testConfig(), env(secrets))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	upsConf := files["ups.conf"]
	for _, want := range []string{"[ups-a]", "driver = snmp-ups", "port = rmcard.host", "secName = snmp1", "authPassword = authpw", "privPassword = privpw", "[shed-p1]", "[shed-p2]", "driver = dummy-ups"} {
		if !strings.Contains(upsConf, want) {
			t.Errorf("ups.conf missing %q\n%s", want, upsConf)
		}
	}
	// Remote UPS must NOT get a local driver section.
	if strings.Contains(upsConf, "[nut]") {
		t.Errorf("remote ups-b should have no local driver section:\n%s", upsConf)
	}

	users := files["upsd.users"]
	for _, want := range []string{"[nutdog]", "password = adminpw", "actions = SET", "[p1]", "password = p1pw", "[p2]", "password = p2pw", "upsmon secondary"} {
		if !strings.Contains(users, want) {
			t.Errorf("upsd.users missing %q\n%s", want, users)
		}
	}

	if files["upsd.conf"] != "LISTEN 0.0.0.0 3493\n" {
		t.Errorf("upsd.conf = %q", files["upsd.conf"])
	}
	if files["nut.conf"] != "MODE=netserver\n" {
		t.Errorf("nut.conf = %q (must enable NUT)", files["nut.conf"])
	}

	for _, f := range []string{"shed-p1.dev", "shed-p2.dev"} {
		if !strings.Contains(files[f], "ups.status: OL") {
			t.Errorf("%s missing baseline status", f)
		}
	}
}

func TestRenderErrorsOnMissingSecret(t *testing.T) {
	// P2_PW deliberately absent.
	secrets := map[string]string{"ADMIN_PW": "x", "SNMP_AUTH": "x", "SNMP_PRIV": "x", "P1_PW": "x"}
	if _, err := Render(testConfig(), env(secrets)); err == nil {
		t.Error("expected error when a referenced secret env is empty")
	}
}

func TestRenderRejectsNewlineInSecret(t *testing.T) {
	// A newline in a rendered secret could inject NUT directives / user stanzas.
	secrets := map[string]string{
		"ADMIN_PW": "x", "SNMP_AUTH": "x", "SNMP_PRIV": "x",
		"P1_PW": "ok", "P2_PW": "bad\n[evil]\n    password = pwned",
	}
	if _, err := Render(testConfig(), env(secrets)); err == nil {
		t.Error("expected error when a secret contains a newline")
	}
}
