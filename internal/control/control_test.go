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
		ext    Request
		want   Desired
	}{
		{"single shed -> off", []SourceState{SourceShed}, NoRequest, DesiredOff},
		{"single healthy -> on", []SourceState{SourceHealthy}, NoRequest, DesiredOn},
		{"any shed wins over healthy", []SourceState{SourceHealthy, SourceShed}, NoRequest, DesiredOff},
		{"shed wins even over unknown", []SourceState{SourceShed, SourceUnknown}, NoRequest, DesiredOff},
		{"unknown without shed holds (fail safe)", []SourceState{SourceHealthy, SourceUnknown}, NoRequest, DesiredHold},
		{"healthy + hold -> hold", []SourceState{SourceHealthy, SourceHold}, NoRequest, DesiredHold},
		{"all healthy -> on", []SourceState{SourceHealthy, SourceHealthy}, NoRequest, DesiredOn},
		{"lone hold -> hold", []SourceState{SourceHold}, NoRequest, DesiredHold},

		// External requests. The UPS outranks them in one direction only.
		{"requested off while healthy -> off", []SourceState{SourceHealthy}, RequestOff, DesiredOff},
		{"requested off while unknown -> off", []SourceState{SourceUnknown}, RequestOff, DesiredOff},
		{"requested on while healthy -> on", []SourceState{SourceHealthy}, RequestOn, DesiredOn},
		// The wake interlock: never power on into a UPS that isn't clearly healthy.
		{"requested on while shed -> off", []SourceState{SourceShed}, RequestOn, DesiredOff},
		{"requested on while unknown -> hold", []SourceState{SourceHealthy, SourceUnknown}, RequestOn, DesiredHold},
		{"requested on while one shed -> off", []SourceState{SourceHealthy, SourceShed}, RequestOn, DesiredOff},

		// An explicit hold is not the same as no request: no request hands the load
		// back to the UPS verdict, which powers it on. Hold pins it where it is, which
		// is what keeps a shed the caller just asked for from being undone mid-flight.
		{"hold while healthy -> hold, not on", []SourceState{SourceHealthy, SourceHealthy}, RequestHold, DesiredHold},
		{"no request while healthy -> on", []SourceState{SourceHealthy, SourceHealthy}, NoRequest, DesiredOn},
		{"hold never blocks a UPS shed", []SourceState{SourceHealthy, SourceShed}, RequestHold, DesiredOff},
	}
	for _, tt := range tests {
		if got := DesiredForLoad(tt.states, tt.ext); got != tt.want {
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
		shed   ShedState
		want   []Action
	}{
		{"chassis shed when up", Chassis, DesiredOff, ActualUp, ShedUnknown, []Action{{ChassisShutdown, "L"}}},
		{"chassis no reshed when down", Chassis, DesiredOff, ActualDown, ShedUnknown, nil},
		{"chassis powerup when down", Chassis, DesiredOn, ActualDown, ShedUnknown, []Action{{ChassisPowerUp, "L"}}},
		{"chassis no powerup when up", Chassis, DesiredOn, ActualUp, ShedUnknown, nil},
		{"chassis hold does nothing", Chassis, DesiredHold, ActualUp, ShedUnknown, nil},
		// NUT server, edge-triggered on the shed signal's observed position.
		{"server asserts when released and off", NutServer, DesiredOff, ActualUp, ShedReleased, []Action{{AssertServerShed, "L"}}},
		{"server no re-assert when already asserted", NutServer, DesiredOff, ActualDown, ShedAsserted, nil},
		{"server asserts on unknown signal (fail-safe shed)", NutServer, DesiredOff, ActualUp, ShedUnknown, []Action{{AssertServerShed, "L"}}},
		{"server release + wake when asserted and down", NutServer, DesiredOn, ActualDown, ShedAsserted, []Action{{ReleaseServerShed, "L"}, {WakeServer, "L"}}},
		{"server release only when asserted and up", NutServer, DesiredOn, ActualUp, ShedAsserted, []Action{{ReleaseServerShed, "L"}}},
		{"server silent when already released and up", NutServer, DesiredOn, ActualUp, ShedReleased, nil},
		{"server wakes only when released but still down", NutServer, DesiredOn, ActualDown, ShedReleased, []Action{{WakeServer, "L"}}},
		{"server hold does nothing", NutServer, DesiredHold, ActualDown, ShedAsserted, nil},
	}
	for _, tt := range tests {
		got := ReconcileLoad("L", tt.lt, tt.d, tt.actual, tt.shed)
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
		shed   map[string]ShedState
		ext    map[string]Request
		want   []Action
	}{
		{
			name:   "grid loss: UPS-A critical sheds BC1 + p1",
			tel:    map[string]Telemetry{"ups-a": onBattery(120, 60), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased}, // was healthy -> assert now
			want:   []Action{{ChassisShutdown, "bc1"}, {AssertServerShed, "p1"}},
		},
		{
			name:   "UPS-B fault sheds p1 only, never BC1",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": onBatteryLow()},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased},
			want:   []Action{{AssertServerShed, "p1"}},
		},
		{
			name:   "recovery: both healthy, both down -> power back up",
			tel:    map[string]Telemetry{"ups-a": online(80), "ups-b": online(90)},
			actual: map[string]ActualState{"bc1": ActualDown, "p1": ActualDown},
			shed:   map[string]ShedState{"p1": ShedAsserted}, // was shed -> release + wake
			want:   []Action{{ChassisPowerUp, "bc1"}, {ReleaseServerShed, "p1"}, {WakeServer, "p1"}},
		},
		{
			name:   "steady state: all healthy, all up, signal released -> nothing",
			tel:    map[string]Telemetry{"ups-a": online(100), "ups-b": online(100)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased},
			want:   nil, // edge-triggered: no re-release of an already-clear signal
		},
		{
			name:   "UPS-A no data: fail safe, do nothing",
			tel:    map[string]Telemetry{"ups-a": noData(), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased},
			want:   nil,
		},
		{
			name:   "deadband: UPS-A on battery but above floor -> hold",
			tel:    map[string]Telemetry{"ups-a": onBattery(600, 80), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased},
			want:   nil,
		},
		{
			name:   "p1 stays shed silently while UPS-B critical even as UPS-A recovers",
			tel:    map[string]Telemetry{"ups-a": online(80), "ups-b": onBatteryLow()},
			actual: map[string]ActualState{"bc1": ActualDown, "p1": ActualDown},
			shed:   map[string]ShedState{"p1": ShedAsserted}, // already asserted
			// BC1 recovers (UPS-A healthy); p1 held down by UPS-B, signal already
			// asserted -> nothing to re-drive.
			want: []Action{{ChassisPowerUp, "bc1"}},
		},
		{
			name:   "solar shed: healthy UPSes, energy-watchdog wants p1 off",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased},
			ext:    map[string]Request{"p1": RequestOff},
			want:   []Action{{AssertServerShed, "p1"}}, // bc1 untouched: no request for it
		},
		{
			name:   "solar wake: request clears, p1 comes back",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualDown},
			shed:   map[string]ShedState{"p1": ShedAsserted},
			ext:    map[string]Request{"p1": RequestOn},
			want:   []Action{{ReleaseServerShed, "p1"}, {WakeServer, "p1"}},
		},
		{
			name:   "UPS critical outranks a request to power p1 on",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": onBatteryLow()},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedReleased},
			ext:    map[string]Request{"p1": RequestOn},
			want:   []Action{{AssertServerShed, "p1"}},
		},
		{
			// The shed is asserted and p1 hasn't finished shutting down yet. Without an
			// explicit hold, healthy UPSes would make nut-dog release the signal and undo
			// the shed the caller just asked for.
			name:   "an explicit hold does not undo a shed in flight",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedAsserted},
			ext:    map[string]Request{"p1": RequestHold},
			want:   nil,
		},
		{
			// Same state with nobody asking: the UPS verdict applies and p1 comes back.
			name:   "no request leaves the same state to the UPSes, which release it",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": online(95)},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualUp},
			shed:   map[string]ShedState{"p1": ShedAsserted},
			want:   []Action{{ReleaseServerShed, "p1"}},
		},
		{
			name:   "request to power on is held while a UPS reads unknown",
			tel:    map[string]Telemetry{"ups-a": online(95), "ups-b": noData()},
			actual: map[string]ActualState{"bc1": ActualUp, "p1": ActualDown},
			shed:   map[string]ShedState{"p1": ShedAsserted},
			ext:    map[string]Request{"p1": RequestOn},
			want:   nil,
		},
	}
	for _, tt := range tests {
		got := Decide(fixtureCfgs(), tt.tel, fixtureLoads(), tt.actual, tt.shed, tt.ext)
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
