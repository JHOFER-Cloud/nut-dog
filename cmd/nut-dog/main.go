// Command nut-dog is the UPS shed/recover controller: a stateless reconcile loop
// that reads both UPSes and the actual power state of each load, then drives BC1
// (chassis via racadm/SSH) and NUT servers (shed signal + WoL) toward the state
// the telemetry implies. All behaviour comes from the mounted config; secrets
// (UPS creds, CMC key, local upsd admin) come from the environment.
package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/app"
	"github.com/JHOFER-Cloud/nut-dog/internal/config"
	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/JHOFER-Cloud/nut-dog/internal/effects"
	"github.com/JHOFER-Cloud/nut-dog/internal/metrics"
	"github.com/JHOFER-Cloud/nut-dog/internal/nut"
	"github.com/JHOFER-Cloud/nut-dog/internal/nutconf"
)

const ioTimeout = 5 * time.Second

// defaultMetricsListen is distinct from energy-watchdog's :9333 so the two can
// coexist on the same hostNetwork control-plane node.
const defaultMetricsListen = ":9334"

// version is stamped at build time (-ldflags "-X main.version=..."); build_info
// carries it.
var version = "dev"

func main() {
	cfgPath := flag.String("config", "/config/config.yaml", "path to config file")
	renderNut := flag.String("render-nut", "", "render the NUT server config from config to this dir and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	// Entrypoint calls this before starting upsd: generate ups.conf / upsd.users /
	// per-server shed files from config, then exit.
	if *renderNut != "" {
		files, err := nutconf.Render(cfg, os.Getenv)
		if err != nil {
			log.Error("render nut config", "err", err)
			os.Exit(1)
		}
		if err := nutconf.WriteFiles(*renderNut, files); err != nil {
			log.Error("write nut config", "err", err)
			os.Exit(1)
		}
		return
	}

	m := metrics.New(version)
	m.SetDryRun(cfg.DryRun)

	poller := newPoller(cfg, m, log)

	// One RacadmChassis (SSH) shared by the chassis prober and the executor.
	var racadm effects.RacadmChassis
	chassisReady := false
	if hasChassis(cfg) {
		// Secret stores mangle multi-line PEMs into literal "\n"; unescape so the
		// key parses however it was stored.
		key := []byte(strings.ReplaceAll(os.Getenv("CMC_SSH_KEY"), `\n`, "\n"))
		hostKey := cmcHostKey(cfg)
		runner, err := effects.NewSSHRunner(cmcUser(cfg), key, hostKey, ioTimeout)
		switch {
		case err != nil && cfg.DryRun:
			// In observe-only mode, keep running so UPS telemetry is still visible.
			log.Warn("cmc ssh setup failed; chassis control disabled in dryRun", "err", err)
		case err != nil:
			log.Error("cmc ssh setup", "err", err)
			os.Exit(1)
		default:
			if hostKey == "" {
				log.Warn("cmc host key not pinned; SSH host-key verification disabled (set cmc.hostKey)")
			}
			racadm = effects.RacadmChassis{R: runner}
			chassisReady = true
		}
	}

	prober, targets, shedUps := wireLoads(cfg, racadm, chassisReady, log)

	localOpts := nut.Options{
		Username: cfg.LocalUpsd.AdminUser,
		Password: os.Getenv(cfg.LocalUpsd.AdminPasswordEnv),
	}
	shedder := effects.NUTShedder{
		ShedUps: shedUps,
		Set:     effects.LocalVarSetter(localUpsdAddr(cfg), localOpts, ioTimeout),
	}

	// Leave Chassis nil unless the CMC runner is ready, so the executor's nil-guard
	// catches a mis-wiring instead of calling into a zero RacadmChassis.
	var chassisEffect effects.Chassis
	if chassisReady {
		chassisEffect = racadm
	}

	executor := &effects.Executor{
		DryRun:  cfg.DryRun,
		Targets: targets,
		Chassis: chassisEffect,
		Shedder: shedder,
		Waker:   effects.UDPWaker{},
		Metrics: m,
		Log:     log,
	}

	ctrl := app.New(cfg.ControlUPS(), cfg.ControlLoads(), poller, prober, executor, log)
	ctrl.Metrics = m
	ctrl.Verbose = cfg.Verbose // opt-in per-tick telemetry log (debug; real telemetry belongs in metrics)
	// Read each shed signal back from the local upsd so the reconcile only drives
	// it on a real transition (edge-triggered), rather than re-asserting every tick.
	ctrl.ShedReader = shedReader{
		addr: localUpsdAddr(cfg), opts: localOpts, timeout: ioTimeout,
		shedUps: shedUps, log: log,
	}

	preflight(cfg, poller, racadm, chassisReady, prober, log)

	interval := time.Duration(cfg.PollInterval)
	log.Info("nut-dog started", "pollInterval", interval.String(), "dryRun", cfg.DryRun)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := startMetricsServer(metricsListen(cfg), m, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	ctrl.Tick() // reconcile immediately, don't wait a full interval
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), ioTimeout)
			_ = srv.Shutdown(shutdownCtx)
			cancel()
			return
		case <-ticker.C:
			ctrl.Tick()
		}
	}
}

// startMetricsServer serves /metrics and /healthz in the background and returns
// the server so the run loop can shut it down. A listen failure is logged, not
// fatal — losing observability must not take down the controller.
func startMetricsServer(addr string, m *metrics.Metrics, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: ioTimeout}
	go func() {
		log.Info("metrics listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server", "err", err)
		}
	}()
	return srv
}

// metricsListen resolves the metrics listen address, defaulting when unset.
func metricsListen(cfg *config.Config) string {
	if cfg.MetricsListen == "" {
		return defaultMetricsListen
	}
	return cfg.MetricsListen
}

// preflight exercises every external path once at startup and logs a ✓/✗ per
// endpoint, so a routing/cred/privilege problem surfaces now instead of during
// an outage. Every check is read-only or a proven no-op (WoL only to an already-
// up host; upsd auth without a SET). It's informational — failures are logged,
// not fatal (the reconcile loop fail-safes regardless).
func preflight(cfg *config.Config, poller nutPoller, racadm effects.RacadmChassis, chassisReady bool, prober funcProber, log *slog.Logger) {
	log.Info("preflight: exercising all endpoints once")
	fails := 0
	report := func(name string, err error) {
		if err != nil {
			log.Warn("preflight ✗", "check", name, "err", err)
			fails++
		} else {
			log.Info("preflight ✓", "check", name)
		}
	}

	for _, name := range sortedKeys(cfg.UPSes) {
		if poller.Poll(name).OK {
			log.Info("preflight ✓", "check", "ups:"+name)
		} else {
			log.Warn("preflight ✗", "check", "ups:"+name, "err", "poll returned no data")
			fails++
		}
	}

	// Local upsd admin auth = the shed-signal SET path (creds + reachability),
	// without touching ups.status.
	if cfg.LocalUpsd.AdminUser != "" {
		report("upsd-admin-auth", nut.CheckAuth(localUpsdAddr(cfg), nut.Options{
			Username: cfg.LocalUpsd.AdminUser,
			Password: os.Getenv(cfg.LocalUpsd.AdminPasswordEnv),
		}, ioTimeout))
	}

	waker := effects.UDPWaker{}
	for _, name := range sortedKeys(cfg.Loads) {
		l := cfg.Loads[name]
		switch l.Type {
		case config.TypeChassis:
			if !chassisReady {
				log.Warn("preflight ✗", "check", "cmc:"+name, "err", "ssh runner not ready")
				fails++
				continue
			}
			_, err := racadm.PowerState(l.CMC.Host)
			report("cmc:"+name, err)
		case config.TypeNutServer:
			actual := prober.Probe(name)
			if actual == control.ActualUnknown {
				log.Warn("preflight ✗", "check", "probe:"+name, "err", "unreachable")
				fails++
			} else {
				log.Info("preflight ✓", "check", "probe:"+name, "state", actual == control.ActualUp)
			}
			// WoL only to an up host (no-op); never wake a down host at startup.
			if actual == control.ActualUp {
				report("wol:"+name, waker.Wake(l.Wake.MAC, l.Wake.Broadcast))
			} else {
				log.Info("preflight — skip", "check", "wol:"+name, "reason", "target not up")
			}
		}
	}

	if fails > 0 {
		log.Warn("preflight complete with failures", "failures", fails)
	} else {
		log.Info("preflight complete: all endpoints reachable")
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- poller ---

type upsPoll struct {
	addr string
	ups  string
	opts nut.Options
}

type nutPoller struct {
	specs   map[string]upsPoll
	timeout time.Duration
	metrics *metrics.Metrics
	log     *slog.Logger
}

// Poll never errors: a failed read returns zero Telemetry (OK=false), which the
// controller treats as Unknown and fail-safes on. It also records the raw UPS
// telemetry + freshness as a side effect, since this is where the full var map
// lives.
func (p nutPoller) Poll(ups string) control.Telemetry {
	spec, ok := p.specs[ups]
	if !ok {
		return control.Telemetry{}
	}
	vars, err := nut.Fetch(spec.addr, spec.ups, spec.opts, p.timeout)
	if err != nil {
		p.log.Warn("ups poll failed", "ups", ups, "err", err)
		p.metrics.RecordPoll(ups, false, nil)
		return control.Telemetry{}
	}
	p.metrics.RecordPoll(ups, true, vars)
	return nut.TelemetryFromVars(vars)
}

func newPoller(cfg *config.Config, m *metrics.Metrics, log *slog.Logger) nutPoller {
	specs := make(map[string]upsPoll, len(cfg.UPSes))
	for name, u := range cfg.UPSes {
		specs[name] = upsPoll{
			addr: u.Host,
			ups:  u.UPSName,
			opts: nut.Options{
				TLS:                u.TLS,
				InsecureSkipVerify: u.Insecure,
				Username:           os.Getenv(u.UsernameEnv),
				Password:           os.Getenv(u.PasswordEnv),
			},
		}
	}
	return nutPoller{specs: specs, timeout: ioTimeout, metrics: m, log: log}
}

// --- prober ---

type funcProber map[string]func() control.ActualState

func (p funcProber) Probe(load string) control.ActualState {
	if fn, ok := p[load]; ok {
		return fn()
	}
	return control.ActualUnknown
}

// wireLoads builds the per-load probe functions, executor targets, and shed-ups
// map from config. Chassis loads probe via getmodinfo over SSH; nut-server loads
// probe via a TCP reachability check.
func wireLoads(cfg *config.Config, chassis effects.RacadmChassis, chassisReady bool, log *slog.Logger) (funcProber, map[string]effects.Target, map[string]string) {
	probes := make(funcProber, len(cfg.Loads))
	targets := make(map[string]effects.Target, len(cfg.Loads))
	shedUps := make(map[string]string)

	for name, l := range cfg.Loads {
		switch l.Type {
		case config.TypeChassis:
			host := l.CMC.Host
			probes[name] = func() control.ActualState {
				if !chassisReady {
					return control.ActualUnknown // no CMC runner (e.g. bad key in dryRun)
				}
				st, err := chassis.PowerState(host)
				if err != nil {
					// Unknown -> the controller won't act on BC1 this tick (it can't
					// reach the CMC to act anyway); surface it rather than hiding it.
					log.Warn("chassis power probe failed", "load", name, "cmc", host, "err", err)
				}
				return st
			}
			targets[name] = effects.Target{CMCHost: host}
		case config.TypeNutServer:
			addr := l.Probe.Host
			probes[name] = func() control.ActualState { return tcpProbe(addr) }
			targets[name] = effects.Target{WakeMAC: l.Wake.MAC, WakeBcast: l.Wake.Broadcast}
			shedUps[name] = config.ShedUpsName(name)
		}
	}
	return probes, targets, shedUps
}

// --- shed reader ---

// shedReader reads a NUT server's shed signal back from nut-dog's own upsd and
// classifies it, so the controller can stay edge-triggered. A failed read is
// ShedUnknown, which makes the reconcile re-drive the signal (safe fallback).
type shedReader struct {
	addr    string
	opts    nut.Options
	timeout time.Duration
	shedUps map[string]string // load name -> shed dummy-ups name
	log     *slog.Logger
}

func (r shedReader) ReadShed(load string) control.ShedState {
	ups, ok := r.shedUps[load]
	if !ok {
		return control.ShedUnknown
	}
	vars, err := nut.Fetch(r.addr, ups, r.opts, r.timeout)
	if err != nil {
		r.log.Warn("shed signal read failed", "load", load, "ups", ups, "err", err)
		return control.ShedUnknown
	}
	return effects.ParseShedStatus(vars["ups.status"])
}

func tcpProbe(addr string) control.ActualState {
	conn, err := net.DialTimeout("tcp", addr, ioTimeout)
	if err != nil {
		return control.ActualDown
	}
	_ = conn.Close()
	return control.ActualUp
}

// --- helpers ---

// localUpsdAddr is where the shedder reaches this pod's own upsd (loopback).
func localUpsdAddr(cfg *config.Config) string {
	_, port, err := net.SplitHostPort(cfg.LocalUpsd.Listen)
	if err != nil || port == "" {
		port = "3493"
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func hasChassis(cfg *config.Config) bool {
	for _, l := range cfg.Loads {
		if l.Type == config.TypeChassis {
			return true
		}
	}
	return false
}

// cmcUser returns the SSH user for the CMC (all chassis loads share one CMC user
// in practice); defaults to "service", the only key-auth account on the M1000e.
func cmcUser(cfg *config.Config) string {
	for _, l := range cfg.Loads {
		if l.Type == config.TypeChassis && l.CMC != nil && l.CMC.SSHUser != "" {
			return l.CMC.SSHUser
		}
	}
	return "service"
}

// cmcHostKey returns the pinned CMC host key from the first chassis load, if set.
func cmcHostKey(cfg *config.Config) string {
	for _, l := range cfg.Loads {
		if l.Type == config.TypeChassis && l.CMC != nil && l.CMC.HostKey != "" {
			return l.CMC.HostKey
		}
	}
	return ""
}
