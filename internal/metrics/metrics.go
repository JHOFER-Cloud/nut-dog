// Package metrics exposes nut-dog's Prometheus surface. It is the observability
// counterpart to the stateless controller: because nut-dog does nothing on
// missing telemetry (fail-safe), the freshness gauges are what turn a silent
// "no data, so no action" into something alertable. It also re-exports the full
// UPS telemetry nut-dog already polls, so a single scrape covers both the
// controller's decisions and the raw UPS readings (load, voltages, temperature).
//
// Recording is split by the layer that naturally holds each datum: the poller
// records raw UPS telemetry + freshness, the controller records its
// interpretation (source classification, per-load desired/actual/shed), and the
// executor records the actions it dispatched and any that failed. Every recorder
// is a no-op on a nil *Metrics, so the pure packages stay testable without it.
package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// telemetrySpecs maps a NUT variable to a dedicated gauge. Each is emitted only
// when the UPS actually reports it, so the set adapts to what CyberPower vs
// UniFi expose without per-UPS code. scale converts NUT's unit to the metric's
// (all 1 today; kept for e.g. milli-units a future UPS might report).
//
// Grounded in the live var sets: the CyberPower RMCARD reports output.realpower
// (watts) but no temperature; the UniFi reports output.power (VA) and
// ups.temperature. Every entry here is reported by at least one of them.
var telemetrySpecs = []struct {
	varName, metric, help string
	scale                 float64
}{
	{"battery.charge", "nut_dog_ups_battery_charge_percent", "Battery charge, percent.", 1},
	{"battery.runtime", "nut_dog_ups_battery_runtime_seconds", "Estimated battery runtime, seconds.", 1},
	{"battery.voltage", "nut_dog_ups_battery_voltage_volts", "Battery voltage, volts.", 1},
	{"input.voltage", "nut_dog_ups_input_voltage_volts", "Input (mains) voltage, volts.", 1},
	{"input.frequency", "nut_dog_ups_input_frequency_hertz", "Input frequency, hertz.", 1},
	{"input.current", "nut_dog_ups_input_current_amperes", "Input current, amperes.", 1},
	{"output.voltage", "nut_dog_ups_output_voltage_volts", "Output voltage, volts.", 1},
	{"output.frequency", "nut_dog_ups_output_frequency_hertz", "Output frequency, hertz.", 1},
	{"output.current", "nut_dog_ups_output_current_amperes", "Output current, amperes.", 1},
	{"output.power", "nut_dog_ups_output_power_voltamperes", "Output apparent power, volt-amperes (UniFi).", 1},
	{"output.realpower", "nut_dog_ups_output_realpower_watts", "Output real power, watts (CyberPower).", 1},
	{"ups.load", "nut_dog_ups_load_percent", "UPS load, percent of capacity.", 1},
	{"ups.temperature", "nut_dog_ups_temperature_celsius", "UPS temperature, celsius (UniFi).", 1},
}

// statusFlags is the curated ups.status token set we surface as 0/1. Fixed (not
// derived from the reading) so a flag that clears is reset to 0 rather than
// lingering — the whole set is written every poll. TEST/CAL are included because
// the CyberPower reports "OL TEST" during its scheduled self-test.
var statusFlags = []string{"OL", "OB", "LB", "RB", "CHRG", "DISCHRG", "BYPASS", "CAL", "TEST", "OVER", "TRIM", "BOOST", "OFF", "ALARM"}

// Enum label value sets for the interpretation gauges. The active value is set
// to 1 and the rest to 0 each tick, so exactly one is hot per series.
var (
	sourceStates  = []string{"unknown", "shed", "hold", "healthy"}
	desiredStates = []string{"hold", "off", "on"}
	actualStates  = []string{"unknown", "up", "down"}
	shedStates    = []string{"unknown", "released", "asserted"}
)

// Metrics holds every collector plus the registry they serve on.
type Metrics struct {
	reg *prometheus.Registry

	// UPS telemetry + freshness (poller).
	upsReachable   *prometheus.GaugeVec
	upsLastSuccess *prometheus.GaugeVec
	upsPollFails   *prometheus.CounterVec
	upsStatus      *prometheus.GaugeVec
	telemetry      map[string]*prometheus.GaugeVec // varName -> gauge

	// Interpretation (controller).
	upsSource     *prometheus.GaugeVec
	loadDesired   *prometheus.GaugeVec
	loadActual    *prometheus.GaugeVec
	loadShed      *prometheus.GaugeVec
	reconcileTime prometheus.Gauge

	// Actions (executor) + mode.
	actions     *prometheus.CounterVec
	actionFails *prometheus.CounterVec
	dryRun      prometheus.Gauge
	buildInfo   *prometheus.GaugeVec
}

// New builds and registers every collector on a private registry (plus the Go
// and process collectors for the pod itself) and records build_info.
func New(version string) *Metrics {
	m := &Metrics{
		reg:       prometheus.NewRegistry(),
		telemetry: make(map[string]*prometheus.GaugeVec, len(telemetrySpecs)),
	}

	m.upsReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_ups_reachable", Help: "1 if the last poll of this UPS succeeded, else 0.",
	}, []string{"ups"})
	m.upsLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_ups_last_success_timestamp_seconds", Help: "Unix time of the last successful poll of this UPS.",
	}, []string{"ups"})
	m.upsPollFails = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nut_dog_ups_poll_failures_total", Help: "Count of failed polls per UPS.",
	}, []string{"ups"})
	m.upsStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_ups_status", Help: "ups.status flags, 1 if present this poll else 0.",
	}, []string{"ups", "flag"})
	m.upsSource = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_ups_source_state", Help: "nut-dog's classification of the UPS (one-hot).",
	}, []string{"ups", "state"})
	m.loadDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_load_desired_state", Help: "Desired power state per load (one-hot).",
	}, []string{"load", "state"})
	m.loadActual = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_load_actual_state", Help: "Observed power state per load (one-hot).",
	}, []string{"load", "state"})
	m.loadShed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_load_shed_signal", Help: "Shed-signal position per NUT-server load (one-hot).",
	}, []string{"load", "state"})
	m.reconcileTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "nut_dog_reconcile_timestamp_seconds", Help: "Unix time of the last completed reconcile tick.",
	})
	m.actions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nut_dog_actions_total", Help: "Actions the controller decided to apply (counted even in dryRun).",
	}, []string{"load", "action"})
	m.actionFails = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nut_dog_action_failures_total", Help: "Actions whose execution failed (armed only).",
	}, []string{"load", "action"})
	m.dryRun = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "nut_dog_dry_run", Help: "1 if running in dryRun (observe-only), 0 if armed.",
	})
	m.buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nut_dog_build_info", Help: "Build info; constant 1 labelled by version.",
	}, []string{"version"})

	cs := []prometheus.Collector{
		m.upsReachable, m.upsLastSuccess, m.upsPollFails, m.upsStatus,
		m.upsSource, m.loadDesired, m.loadActual, m.loadShed, m.reconcileTime,
		m.actions, m.actionFails, m.dryRun, m.buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	}
	for _, spec := range telemetrySpecs {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: spec.metric, Help: spec.help}, []string{"ups"})
		m.telemetry[spec.varName] = g
		cs = append(cs, g)
	}
	m.reg.MustRegister(cs...)

	m.buildInfo.WithLabelValues(version).Set(1)
	return m
}

// Handler serves the registry at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// SetDryRun records observe-only vs armed so a stuck-in-dryRun deploy is visible.
func (m *Metrics) SetDryRun(dry bool) {
	if m == nil {
		return
	}
	m.dryRun.Set(b2f(dry))
}

// RecordPoll records one UPS poll. On failure it updates only reachability and
// the failure counter, leaving the last-known telemetry (and last_success)
// untouched — reachable=0 + a stale timestamp is the signal, not fake zeros.
func (m *Metrics) RecordPoll(ups string, ok bool, vars map[string]string) {
	if m == nil {
		return
	}
	m.upsReachable.WithLabelValues(ups).Set(b2f(ok))
	if !ok {
		m.upsPollFails.WithLabelValues(ups).Inc()
		return
	}
	m.upsLastSuccess.WithLabelValues(ups).Set(float64(time.Now().Unix()))

	present := tokenSet(vars["ups.status"])
	for _, flag := range statusFlags {
		m.upsStatus.WithLabelValues(ups, flag).Set(b2f(present[flag]))
	}
	for _, spec := range telemetrySpecs {
		raw, has := vars[spec.varName]
		if !has {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			m.telemetry[spec.varName].WithLabelValues(ups).Set(v * spec.scale)
		}
	}
}

// RecordSource records the controller's classification of a UPS.
func (m *Metrics) RecordSource(ups string, s control.SourceState) {
	if m == nil {
		return
	}
	setOneHot(m.upsSource, ups, sourceStates, sourceString(s))
}

// RecordLoad records the per-load decision inputs. The shed signal is only
// meaningful for NUT servers; for a chassis it is left unset.
func (m *Metrics) RecordLoad(load string, d control.Desired, a control.ActualState, sh control.ShedState, nutServer bool) {
	if m == nil {
		return
	}
	setOneHot(m.loadDesired, load, desiredStates, desiredString(d))
	setOneHot(m.loadActual, load, actualStates, actualString(a))
	if nutServer {
		setOneHot(m.loadShed, load, shedStates, shedString(sh))
	}
}

// ObserveReconcile stamps the completion of a reconcile tick (loop heartbeat).
func (m *Metrics) ObserveReconcile() {
	if m == nil {
		return
	}
	m.reconcileTime.Set(float64(time.Now().Unix()))
}

// RecordAction counts one dispatched action and, if it failed, one failure.
func (m *Metrics) RecordAction(load, action string, failed bool) {
	if m == nil {
		return
	}
	m.actions.WithLabelValues(load, action).Inc()
	if failed {
		m.actionFails.WithLabelValues(load, action).Inc()
	}
}

// setOneHot sets active to 1 and every other state to 0 for one id.
func setOneHot(g *prometheus.GaugeVec, id string, states []string, active string) {
	for _, s := range states {
		g.WithLabelValues(id, s).Set(b2f(s == active))
	}
}

// tokenSet splits a space-separated ups.status into a presence set.
func tokenSet(status string) map[string]bool {
	set := make(map[string]bool)
	for tok := range strings.FieldsSeq(status) {
		set[tok] = true
	}
	return set
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func sourceString(s control.SourceState) string {
	switch s {
	case control.SourceShed:
		return "shed"
	case control.SourceHold:
		return "hold"
	case control.SourceHealthy:
		return "healthy"
	default:
		return "unknown"
	}
}

func desiredString(d control.Desired) string {
	switch d {
	case control.DesiredOff:
		return "off"
	case control.DesiredOn:
		return "on"
	default:
		return "hold"
	}
}

func actualString(a control.ActualState) string {
	switch a {
	case control.ActualUp:
		return "up"
	case control.ActualDown:
		return "down"
	default:
		return "unknown"
	}
}

func shedString(s control.ShedState) string {
	switch s {
	case control.ShedReleased:
		return "released"
	case control.ShedAsserted:
		return "asserted"
	default:
		return "unknown"
	}
}
