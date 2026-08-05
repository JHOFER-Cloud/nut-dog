package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordPollTelemetryAndStatus(t *testing.T) {
	m := New("test")
	m.RecordPoll("ups-a", true, map[string]string{
		"ups.status":      "OL TEST", // CyberPower during its self-test
		"battery.charge":  "100",
		"battery.runtime": "900",
		"ups.load":        "34",
		"ups.realpower":   "420",          // CyberPower real power (watts), cyberpower MIB
		"ups.temperature": "not-a-number", // must be skipped, not error
	})

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"reachable", testutil.ToFloat64(m.upsReachable.WithLabelValues("ups-a")), 1},
		{"charge", testutil.ToFloat64(m.telemetry["battery.charge"].WithLabelValues("ups-a")), 100},
		{"runtime", testutil.ToFloat64(m.telemetry["battery.runtime"].WithLabelValues("ups-a")), 900},
		{"load", testutil.ToFloat64(m.telemetry["ups.load"].WithLabelValues("ups-a")), 34},
		{"realpower", testutil.ToFloat64(m.telemetry["ups.realpower"].WithLabelValues("ups-a")), 420},
		{"status OL", testutil.ToFloat64(m.upsStatus.WithLabelValues("ups-a", "OL")), 1},
		{"status TEST", testutil.ToFloat64(m.upsStatus.WithLabelValues("ups-a", "TEST")), 1},
		{"status OB reset to 0", testutil.ToFloat64(m.upsStatus.WithLabelValues("ups-a", "OB")), 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A failed poll updates only reachability + the failure counter, and must not
// clobber the last-known telemetry with fake zeros.
func TestRecordPollFailureKeepsTelemetry(t *testing.T) {
	m := New("test")
	m.RecordPoll("ups-b", true, map[string]string{"ups.status": "OL", "battery.charge": "80"})
	m.RecordPoll("ups-b", false, nil)

	if got := testutil.ToFloat64(m.upsReachable.WithLabelValues("ups-b")); got != 0 {
		t.Errorf("reachable = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.upsPollFails.WithLabelValues("ups-b")); got != 1 {
		t.Errorf("poll_failures = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.telemetry["battery.charge"].WithLabelValues("ups-b")); got != 80 {
		t.Errorf("charge = %v, want 80 (kept from last good poll)", got)
	}
}

// A var that disappears from a *successful* poll must drop its series, not
// freeze at the last value: the 2026-08 UniFi firmware retired output.power.
func TestRecordPollDropsVanishedVar(t *testing.T) {
	m := New("test")
	va := m.telemetry["ups.power"]

	m.RecordPoll("ups-b", true, map[string]string{"ups.status": "OL", "output.power": "138"})
	if got := testutil.ToFloat64(va.WithLabelValues("ups-b")); got != 138 {
		t.Fatalf("VA = %v, want 138", got)
	}

	m.RecordPoll("ups-b", true, map[string]string{"ups.status": "OL"})
	if got := testutil.CollectAndCount(va); got != 0 {
		t.Errorf("VA series count = %v, want 0 (var gone, series dropped)", got)
	}
}

// output.power and ups.power feed one gauge; the newer name wins when both are
// present, so a firmware that reports both does not flip-flop the series.
func TestRecordPollVarNameAliases(t *testing.T) {
	m := New("test")
	va := m.telemetry["ups.power"]

	if m.telemetry["output.power"] != va {
		t.Fatal("output.power and ups.power must share one gauge")
	}
	m.RecordPoll("ups-b", true, map[string]string{"ups.power": "135.4", "output.power": "138"})
	if got := testutil.ToFloat64(va.WithLabelValues("ups-b")); got != 135.4 {
		t.Errorf("VA = %v, want 135.4 (ups.power preferred)", got)
	}
}

// One-hot gauges must flip cleanly: setting a new state zeroes the old one.
func TestOneHotResets(t *testing.T) {
	m := New("test")
	m.RecordSource("ups-a", control.SourceShed)
	if got := testutil.ToFloat64(m.upsSource.WithLabelValues("ups-a", "shed")); got != 1 {
		t.Fatalf("shed = %v, want 1", got)
	}
	m.RecordSource("ups-a", control.SourceHealthy)
	if got := testutil.ToFloat64(m.upsSource.WithLabelValues("ups-a", "healthy")); got != 1 {
		t.Errorf("healthy = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.upsSource.WithLabelValues("ups-a", "shed")); got != 0 {
		t.Errorf("stale shed = %v, want 0 after transition", got)
	}
}

func TestRecordActionCountsFailures(t *testing.T) {
	m := New("test")
	m.RecordAction("bc1", "ChassisShutdown", false)
	m.RecordAction("bc1", "ChassisShutdown", true)
	if got := testutil.ToFloat64(m.actions.WithLabelValues("bc1", "ChassisShutdown")); got != 2 {
		t.Errorf("actions = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.actionFails.WithLabelValues("bc1", "ChassisShutdown")); got != 1 {
		t.Errorf("failures = %v, want 1", got)
	}
}

func TestRecordWakeInhibited(t *testing.T) {
	m := New("test")
	m.RecordWakeInhibited("p1")
	m.RecordWakeInhibited("p1")
	if got := testutil.ToFloat64(m.wakeInhibited.WithLabelValues("p1")); got != 2 {
		t.Errorf("wake_inhibited = %v, want 2", got)
	}
}

// A chassis load has no shed signal; RecordLoad must not create that series.
func TestRecordLoadChassisSkipsShed(t *testing.T) {
	m := New("test")
	m.RecordLoad("bc1", control.DesiredOn, control.ActualUp, control.ShedUnknown, false)
	if got := testutil.CollectAndCount(m.loadShed); got != 0 {
		t.Errorf("shed series for chassis = %d, want 0", got)
	}
	if got := testutil.ToFloat64(m.loadDesired.WithLabelValues("bc1", "on")); got != 1 {
		t.Errorf("desired on = %v, want 1", got)
	}
}

// The /metrics endpoint must serve the recorded nut_dog_* series in exposition
// format — exercises the registry -> Handler path end to end.
func TestHandlerExposesSeries(t *testing.T) {
	m := New("1.2.3")
	m.RecordPoll("ups-a", true, map[string]string{"ups.status": "OL", "battery.charge": "100"})
	m.RecordLoad("p1", control.DesiredOn, control.ActualUp, control.ShedReleased, true)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`nut_dog_ups_battery_charge_percent{ups="ups-a"} 100`,
		`nut_dog_ups_status{flag="OL",ups="ups-a"} 1`,
		`nut_dog_load_desired_state{load="p1",state="on"} 1`,
		`nut_dog_build_info{version="1.2.3"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// Every recorder must be a safe no-op on a nil *Metrics.
func TestNilMetricsNoop(t *testing.T) {
	var m *Metrics
	m.SetDryRun(true)
	m.RecordPoll("x", true, map[string]string{"ups.status": "OL"})
	m.RecordSource("x", control.SourceHealthy)
	m.RecordLoad("x", control.DesiredOn, control.ActualUp, control.ShedReleased, true)
	m.RecordAction("x", "WakeServer", false)
	m.RecordWakeInhibited("x")
	m.ObserveReconcile()
}
