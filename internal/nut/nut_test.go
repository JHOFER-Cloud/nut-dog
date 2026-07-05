package nut

import (
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

func TestParseUPSC(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want control.Telemetry
	}{
		{
			name: "online full charge (the confirmed RMCARD shape)",
			raw: "battery.charge: 100\n" +
				"battery.runtime: 900\n" +
				"ups.status: OL\n",
			want: control.Telemetry{OK: true, Status: control.Status{OnLine: true}, Runtime: 900, Charge: 100},
		},
		{
			name: "on battery, discharging, low",
			raw:  "ups.status: OB DISCHRG LB\nbattery.runtime: 120\nbattery.charge: 20\n",
			want: control.Telemetry{OK: true, Status: control.Status{OnBattery: true, LowBattery: true}, Runtime: 120, Charge: 20},
		},
		{
			name: "missing runtime/charge become -1, still OK",
			raw:  "ups.status: OL\n",
			want: control.Telemetry{OK: true, Status: control.Status{OnLine: true}, Runtime: -1, Charge: -1},
		},
		{
			name: "no status line -> not OK (treated as Unknown downstream)",
			raw:  "battery.charge: 55\n",
			want: control.Telemetry{OK: false, Runtime: -1, Charge: 55},
		},
		{
			name: "empty / garbage -> not OK",
			raw:  "\n  \nnonsense\n",
			want: control.Telemetry{OK: false, Runtime: -1, Charge: -1},
		},
		{
			name: "non-numeric charge is ignored (stays -1)",
			raw:  "ups.status: OL\nbattery.charge: n/a\n",
			want: control.Telemetry{OK: true, Status: control.Status{OnLine: true}, Runtime: -1, Charge: -1},
		},
	}
	for _, tt := range tests {
		got := ParseUPSC(tt.raw)
		if got != tt.want {
			t.Errorf("%s:\n  got  %+v\n  want %+v", tt.name, got, tt.want)
		}
	}
}

// Sanity check that the parser feeds the core the way we expect end to end.
func TestParsedTelemetryClassifies(t *testing.T) {
	cfg := control.UPSConfig{ShedRuntime: 300, RecoverCharge: 5}
	if got := control.Classify(cfg, ParseUPSC("ups.status: OB\nbattery.runtime: 120\nbattery.charge: 40\n")); got != control.SourceShed {
		t.Errorf("on-battery low runtime should shed, got %v", got)
	}
	if got := control.Classify(cfg, ParseUPSC("ups.status: OL\nbattery.charge: 100\n")); got != control.SourceHealthy {
		t.Errorf("online full charge should be healthy, got %v", got)
	}
	if got := control.Classify(cfg, ParseUPSC("")); got != control.SourceUnknown {
		t.Errorf("no data should be unknown, got %v", got)
	}
}
