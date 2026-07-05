// Command nut-dog is the UPS shed/recover controller: a stateless reconcile loop
// that reads both UPSes and the actual power state of each load, then drives BC1
// (chassis via racadm/SSH) and NUT servers (shed signal + WoL) toward the state
// the telemetry implies. All behaviour comes from the mounted config; secrets
// (UPS creds, CMC key, local upsd admin) come from the environment.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/app"
	"github.com/JHOFER-Cloud/nut-dog/internal/config"
	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"github.com/JHOFER-Cloud/nut-dog/internal/effects"
	"github.com/JHOFER-Cloud/nut-dog/internal/nut"
	"github.com/JHOFER-Cloud/nut-dog/internal/nutconf"
)

const ioTimeout = 5 * time.Second

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

	poller := newPoller(cfg, log)

	// One RacadmChassis (SSH) shared by the chassis prober and the executor.
	var racadm effects.RacadmChassis
	haveChassis := hasChassis(cfg)
	if haveChassis {
		key := []byte(os.Getenv("CMC_SSH_KEY"))
		hostKey := cmcHostKey(cfg)
		runner, err := effects.NewSSHRunner(cmcUser(cfg), key, hostKey, ioTimeout)
		if err != nil {
			log.Error("cmc ssh setup", "err", err)
			os.Exit(1)
		}
		if hostKey == "" {
			log.Warn("cmc host key not pinned; SSH host-key verification disabled (set cmc.hostKey)")
		}
		racadm = effects.RacadmChassis{R: runner}
	}

	prober, targets, shedUps := wireLoads(cfg, racadm, log)

	shedder := effects.NUTShedder{
		ShedUps: shedUps,
		Set: effects.LocalVarSetter(localUpsdAddr(cfg), nut.Options{
			Username: cfg.LocalUpsd.AdminUser,
			Password: os.Getenv(cfg.LocalUpsd.AdminPasswordEnv),
		}, ioTimeout),
	}

	// Leave Chassis nil when there's no chassis load, so the executor's nil-guard
	// catches a mis-wiring instead of calling into a zero RacadmChassis.
	var chassisEffect effects.Chassis
	if haveChassis {
		chassisEffect = racadm
	}

	executor := &effects.Executor{
		DryRun:  cfg.DryRun,
		Targets: targets,
		Chassis: chassisEffect,
		Shedder: shedder,
		Waker:   effects.UDPWaker{},
		Log:     log,
	}

	ctrl := app.New(cfg.ControlUPS(), cfg.ControlLoads(), poller, prober, executor, log)

	interval := time.Duration(cfg.PollInterval)
	log.Info("nut-dog started", "pollInterval", interval.String(), "dryRun", cfg.DryRun)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	ctrl.Tick() // reconcile immediately, don't wait a full interval
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-ticker.C:
			ctrl.Tick()
		}
	}
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
	log     *slog.Logger
}

// Poll never errors: a failed read returns zero Telemetry (OK=false), which the
// controller treats as Unknown and fail-safes on.
func (p nutPoller) Poll(ups string) control.Telemetry {
	spec, ok := p.specs[ups]
	if !ok {
		return control.Telemetry{}
	}
	vars, err := nut.Fetch(spec.addr, spec.ups, spec.opts, p.timeout)
	if err != nil {
		p.log.Warn("ups poll failed", "ups", ups, "err", err)
		return control.Telemetry{}
	}
	return nut.TelemetryFromVars(vars)
}

func newPoller(cfg *config.Config, log *slog.Logger) nutPoller {
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
	return nutPoller{specs: specs, timeout: ioTimeout, log: log}
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
func wireLoads(cfg *config.Config, chassis effects.RacadmChassis, log *slog.Logger) (funcProber, map[string]effects.Target, map[string]string) {
	probes := make(funcProber, len(cfg.Loads))
	targets := make(map[string]effects.Target, len(cfg.Loads))
	shedUps := make(map[string]string)

	for name, l := range cfg.Loads {
		switch l.Type {
		case config.TypeChassis:
			host := l.CMC.Host
			probes[name] = func() control.ActualState {
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
