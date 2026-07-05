package effects

import (
	"errors"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

type fakeRunner struct {
	gotHost, gotCmd string
	out             string
	err             error
}

func (f *fakeRunner) Run(host, cmd string) (string, error) {
	f.gotHost, f.gotCmd = host, cmd
	return f.out, f.err
}

func TestChassisCommands(t *testing.T) {
	tests := []struct {
		name    string
		call    func(RacadmChassis, string) error
		wantCmd string
	}{
		{"shutdown", RacadmChassis.Shutdown, "racadm chassisaction -m chassis powerdown"},
		{"powerup", RacadmChassis.PowerUp, "racadm chassisaction -m chassis powerup"},
	}
	for _, tt := range tests {
		r := &fakeRunner{out: "Module power operation successful"}
		if err := tt.call(RacadmChassis{R: r}, "cmc.host"); err != nil {
			t.Errorf("%s: %v", tt.name, err)
		}
		if r.gotHost != "cmc.host" || r.gotCmd != tt.wantCmd {
			t.Errorf("%s: ran %q on %q, want %q", tt.name, r.gotCmd, r.gotHost, tt.wantCmd)
		}
	}
}

func TestChassisPropagatesErrors(t *testing.T) {
	// Transport failure.
	c := RacadmChassis{R: &fakeRunner{err: errors.New("dial timeout")}}
	if err := c.Shutdown("cmc.host"); err == nil {
		t.Error("expected error on transport failure")
	}
	// racadm printed ERROR but exited 0.
	c = RacadmChassis{R: &fakeRunner{out: "ERROR: insufficient privileges"}}
	if err := c.PowerUp("cmc.host"); err == nil {
		t.Error("expected error when racadm reports ERROR")
	}
}

// The exact getmodinfo output captured from the real CMC.
const realGetmodinfo = `<module>        <presence>      <pwrState>      <health>        <svcTag>
Chassis         Present         ON              Not OK          9XL9422
CMC-1           Present         Primary         OK              N/A
Server-1        Present         ON              OK              9CXM2T2
Server-2        Present         OFF             OK              GYCDM73
Server-3        Not Present     N/A             N/A             N/A
KVM             Present         ON              OK              N/A`

func TestParseChassisPower(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want control.ActualState
	}{
		{"real output: server-1 ON -> up", realGetmodinfo, control.ActualUp},
		{
			"all present blades off -> down",
			"Server-1        Present         OFF             OK              x\n" +
				"Server-2        Present         OFF             OK              y\n",
			control.ActualDown,
		},
		{
			"absent blades ignored (Not Present)",
			"Server-1        Not Present     N/A             N/A             N/A\n" +
				"Server-2        Not Present     N/A             N/A             N/A\n",
			control.ActualDown,
		},
		{"no server rows -> unknown", "Chassis Present ON OK x\n", control.ActualUnknown},
	}
	for _, tt := range tests {
		if got := parseChassisPower(tt.in); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
