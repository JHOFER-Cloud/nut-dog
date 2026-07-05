package control

import (
	"reflect"
	"testing"
)

var cfg = UPSConfig{ShedRuntime: 300, ShedCharge: 0, RecoverCharge: 5}

// Telemetry builders.
func online(charge int) Telemetry {
	return Telemetry{OK: true, Status: Status{OnLine: true}, Charge: charge, Runtime: 900}
}

func onBattery(runtime, charge int) Telemetry {
	return Telemetry{OK: true, Status: Status{OnBattery: true}, Runtime: runtime, Charge: charge}
}

func onBatteryLow() Telemetry {
	return Telemetry{OK: true, Status: Status{OnBattery: true, LowBattery: true}, Runtime: 30, Charge: 8}
}

func noData() Telemetry { return Telemetry{OK: false} }

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		c    UPSConfig
		tel  Telemetry
		want SourceState
	}{
		{"no data is unknown", cfg, noData(), SourceUnknown},
		{"on battery + LB sheds", cfg, onBatteryLow(), SourceShed},
		{"on battery under runtime floor sheds", cfg, onBattery(300, 90), SourceShed},
		{"on battery below runtime floor sheds", cfg, onBattery(120, 90), SourceShed},
		{"on battery above runtime floor holds", cfg, onBattery(600, 90), SourceHold},
		{"charge trigger sheds when enabled", UPSConfig{ShedRuntime: 300, ShedCharge: 30, RecoverCharge: 5}, onBattery(600, 25), SourceShed},
		{"charge trigger ignored when zero", cfg, onBattery(600, 5), SourceHold},
		{"online recharged is healthy", cfg, online(80), SourceHealthy},
		{"online at recover floor is healthy", cfg, online(5), SourceHealthy},
		{"online below recover floor holds", UPSConfig{ShedRuntime: 300, RecoverCharge: 70}, online(40), SourceHold},
		{"online with unknown charge is healthy", cfg, Telemetry{OK: true, Status: Status{OnLine: true}, Charge: -1, Runtime: 900}, SourceHealthy},
		{"indeterminate status holds", cfg, Telemetry{OK: true}, SourceHold},
	}
	for _, tt := range tests {
		if got := Classify(tt.c, tt.tel); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDesiredForLoad(t *testing.T) {
	tests := []struct {
		name   string
		states []SourceState
		want   Desired
	}{
		{"single shed -> off", []SourceState{SourceShed}, DesiredOff},
		{"single healthy -> on", []SourceState{SourceHealthy}, DesiredOn},
		{"any shed wins over healthy", []SourceState{SourceHealthy, SourceShed}, DesiredOff},
		{"shed wins even over unknown", []SourceState{SourceShed, SourceUnknown}, DesiredOff},
		{"unknown without shed holds (fail safe)", []SourceState{SourceHealthy, SourceUnknown}, DesiredHold},
		{"healthy + hold -> hold", []SourceState{SourceHealthy, SourceHold}, DesiredHold},
		{"all healthy -> on", []SourceState{SourceHealthy, SourceHealthy}, DesiredOn},
		{"lone hold -> hold", []SourceState{SourceHold}, DesiredHold},
	}
	for _, tt := range tests {
		if got := DesiredForLoad(tt.states); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestReconcileLoad(t *testing.T) {
	tests := []struct {
		name   string
		lt     LoadType
		d      Desired
		actual ActualState
		want   []Action
	}{
		{"chassis shed when up", Chassis, DesiredOff, ActualUp, []Action{{ChassisShutdown, "L"}}},
		{"chassis no reshed when down", Chassis, DesiredOff, ActualDown, nil},
		{"chassis powerup when down", Chassis, DesiredOn, ActualDown, []Action{{ChassisPowerUp, "L"}}},
		{"chassis no powerup when up", Chassis, DesiredOn, ActualUp, nil},
		{"chassis hold does nothing", Chassis, DesiredHold, ActualUp, nil},
		{"server holds shed signal when off", NutServer, DesiredOff, ActualUp, []Action{{AssertServerShed, "L"}}},
		{"server release + wake when down", NutServer, DesiredOn, ActualDown, []Action{{ReleaseServerShed, "L"}, {WakeServer, "L"}}},
		{"server release only when up", NutServer, DesiredOn, ActualUp, []Action{{ReleaseServerShed, "L"}}},
		{"server hold does nothing", NutServer, DesiredHold, ActualDown, nil},
	}
	for _, tt := range tests {
		got := ReconcileLoad("L", tt.lt, tt.d, tt.actual)
		if !actionsEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The realistic HLA1 wiring: BC1 governed by UPS-A only; p1 governed by both.
func fixtureLoads() map[string]LoadConfig {
	return map[string]LoadConfig{
		"bc1": {Type: Chassis, GovernedBy: []string{"ups-a"}},
		"p1":  {Type: NutServer, GovernedBy: []string{"ups-a", "ups-b"}},
	}
}

func fixtureCfgs() map[string]UPSConfig {
	return map[string]UPSConfig{"ups-a": cfg, "ups-b": cfg}
}

func TestDecideScenarios(t *testing.T) {
	tests := []struct {
		name   string
		tel    map[string]Telemetry
		actual map[string]ActualState
		want   []Action
	}{
		{
			name:   "grid loss: UPS-A critical sheds BC1 + p1",
			tel:    map[string]Telemetry{"ups-a": onBattery(120, 60), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			want:   []Action{{ChassisShutdown, "bc1"}, {AssertServerShed, "p1"}},
		},
		{
			name:   "UPS-B fault sheds p1 only, never BC1",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": onBatteryLow()},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			want:   []Action{{AssertServerShed, "p1"}},
		},
		{
			name:   "recovery: both healthy, both down -> power back up",
			tel:    map[string]Telemetry{"ups-a": online(80), "ups-b": online(90)},
			actual: map[string]ActualState{"bc1": ActualDown, "p1": ActualDown},
			want:   []Action{{ChassisPowerUp, "bc1"}, {ReleaseServerShed, "p1"}, {WakeServer, "p1"}},
		},
		{
			name:   "UPS-A no data: fail safe, do nothing",
			tel:    map[string]Telemetry{"ups-a": noData(), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			want:   nil,
		},
		{
			name:   "deadband: UPS-A on battery but above floor -> hold",
			tel:    map[string]Telemetry{"ups-a": onBattery(600, 80), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			want:   nil,
		},
		{
			name:   "p1 stays shed while UPS-B still critical even as UPS-A recovers",
			tel:    map[string]Telemetry{"ups-a": online(80), "ups-b": onBatteryLow()},
			actual: map[string]ActualState{"bc1": ActualDown, "p1": ActualDown},
			// BC1 recovers (UPS-A healthy); p1 held down by UPS-B -> shed signal kept asserted.
			want: []Action{{ChassisPowerUp, "bc1"}, {AssertServerShed, "p1"}},
		},
	}
	for _, tt := range tests {
		got := Decide(fixtureCfgs(), tt.tel, fixtureLoads(), tt.actual)
		if !actionsEqual(got, tt.want) {
			t.Errorf("%s:\n  got  %v\n  want %v", tt.name, got, tt.want)
		}
	}
}

func actionsEqual(a, b []Action) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
