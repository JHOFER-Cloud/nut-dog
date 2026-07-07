// Package config loads nut-dog's runtime configuration from the YAML the fleet
// ConfigMap mounts at /config. Everything tunable lives here — nothing is
// hardcoded in the image. It is the single source of truth for both the pure
// control core (via ControlUPS/ControlLoads) and the NUT server-side config the
// pod runs (rendered by internal/nutconf from the same structs). Adding a UPS or
// a server is a config block, never a code or template change.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a Go duration string ("5m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the whole file.
type Config struct {
	PollInterval Duration            `yaml:"pollInterval"`
	DryRun       bool                `yaml:"dryRun"`
	Verbose      bool                `yaml:"verbose"` // log telemetry + state each tick (debug; noisy)
	LocalUpsd    LocalUpsdSpec       `yaml:"localUpsd"`
	UPSes        map[string]UPSSpec  `yaml:"upses"`
	Loads        map[string]LoadSpec `yaml:"loads"`
}

// LocalUpsdSpec configures this pod's own upsd — the NUT server that serves the
// local drivers and the per-server shed signals to external secondaries.
type LocalUpsdSpec struct {
	Listen           string `yaml:"listen"`           // host:port to listen on (hostNetwork → node IP)
	AdminUser        string `yaml:"adminUser"`        // upsd user with SET rights (the shed writer)
	AdminPasswordEnv string `yaml:"adminPasswordEnv"` // env var holding that user's password
}

// UPSSpec is one UPS: how to reach it, its thresholds, and — if locally driven —
// the NUT driver to run for it.
type UPSSpec struct {
	Host             string      `yaml:"host"`    // host:port of the NUT server nut-dog reads
	UPSName          string      `yaml:"upsName"` // UPS name on that server
	TLS              bool        `yaml:"tls"`
	Insecure         bool        `yaml:"insecure"`
	UsernameEnv      string      `yaml:"usernameEnv"` // env var: monitoring user (remote UPSes)
	PasswordEnv      string      `yaml:"passwordEnv"`
	ShedRuntime      Duration    `yaml:"shedRuntime"`
	ShedChargePct    int         `yaml:"shedChargePct"`
	RecoverChargePct int         `yaml:"recoverChargePct"`
	Driver           *DriverSpec `yaml:"driver"` // present => nut-dog runs this driver locally
}

// DriverSpec describes a local NUT driver (e.g. snmp-ups for a CyberPower RMCARD).
// Options are rendered verbatim; SecretOptions map an option name to the env var
// holding its (secret) value, so passwords never live in the ConfigMap.
type DriverSpec struct {
	Type          string            `yaml:"type"`
	Port          string            `yaml:"port"`
	Options       map[string]string `yaml:"options"`
	SecretOptions map[string]string `yaml:"secretOptions"`
}

// LoadSpec is one controllable load.
type LoadSpec struct {
	Type       string         `yaml:"type"` // "chassis" | "nut-server"
	GovernedBy []string       `yaml:"governedBy"`
	CMC        *CMCSpec       `yaml:"cmc"`       // chassis only
	Wake       *WakeSpec      `yaml:"wake"`      // nut-server only
	Probe      *ProbeSpec     `yaml:"probe"`     // nut-server only
	Secondary  *SecondarySpec `yaml:"secondary"` // nut-server only
}

type CMCSpec struct {
	Host    string `yaml:"host"`
	SSHUser string `yaml:"sshUser"`
	// HostKey pins the CMC's SSH host key (an authorized_keys line, e.g.
	// "ssh-ed25519 AAAA..."). Optional; if empty, host-key verification is
	// disabled (acceptable on a trusted mgmt VLAN, but pinning is better).
	HostKey string `yaml:"hostKey"`
}

type WakeSpec struct {
	MAC       string `yaml:"mac"`
	Broadcast string `yaml:"broadcast"` // host:port for the WoL packet
}

type ProbeSpec struct {
	Host string `yaml:"host"` // host:port to TCP-probe for reachability
}

// SecondarySpec is the NUT login a server's upsmon uses to watch its shed signal.
type SecondarySpec struct {
	User        string `yaml:"user"`
	PasswordEnv string `yaml:"passwordEnv"`
}

const (
	TypeChassis   = "chassis"
	TypeNutServer = "nut-server"
)

// ShedUpsName is the dummy-ups name for a NUT server's shed signal. Derived from
// the load name so it stays consistent between the renderer and the controller.
func ShedUpsName(load string) string { return "shed-" + load }

// Load reads and validates the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if time.Duration(c.PollInterval) <= 0 {
		return fmt.Errorf("pollInterval must be > 0")
	}
	if len(c.UPSes) == 0 {
		return fmt.Errorf("no upses configured")
	}
	for name, u := range c.UPSes {
		if u.Host == "" || u.UPSName == "" {
			return fmt.Errorf("ups %q: host and upsName are required", name)
		}
		if time.Duration(u.ShedRuntime) <= 0 {
			return fmt.Errorf("ups %q: shedRuntime must be > 0", name)
		}
		if u.Driver != nil && (u.Driver.Type == "" || u.Driver.Port == "") {
			return fmt.Errorf("ups %q: driver needs type and port", name)
		}
	}

	nutServers := 0
	for name, l := range c.Loads {
		switch l.Type {
		case TypeChassis:
			if l.CMC == nil || l.CMC.Host == "" {
				return fmt.Errorf("load %q: chassis needs cmc.host", name)
			}
		case TypeNutServer:
			nutServers++
			if l.Wake == nil || l.Wake.MAC == "" || l.Wake.Broadcast == "" {
				return fmt.Errorf("load %q: nut-server needs wake.mac and wake.broadcast", name)
			}
			if l.Probe == nil || l.Probe.Host == "" {
				return fmt.Errorf("load %q: nut-server needs probe.host", name)
			}
			if l.Secondary == nil || l.Secondary.User == "" || l.Secondary.PasswordEnv == "" {
				return fmt.Errorf("load %q: nut-server needs secondary.user and secondary.passwordEnv", name)
			}
		default:
			return fmt.Errorf("load %q: unknown type %q", name, l.Type)
		}
		if len(l.GovernedBy) == 0 {
			return fmt.Errorf("load %q: governedBy is empty", name)
		}
		for _, u := range l.GovernedBy {
			if _, ok := c.UPSes[u]; !ok {
				return fmt.Errorf("load %q: governedBy references unknown ups %q", name, u)
			}
		}
	}

	// The local upsd only matters once something depends on it (a local driver to
	// serve, or a NUT server to feed the shed signal to).
	if nutServers > 0 || c.hasLocalDriver() {
		if c.LocalUpsd.Listen == "" {
			return fmt.Errorf("localUpsd.listen is required when a local driver or nut-server is configured")
		}
	}
	if nutServers > 0 && (c.LocalUpsd.AdminUser == "" || c.LocalUpsd.AdminPasswordEnv == "") {
		return fmt.Errorf("localUpsd.adminUser and adminPasswordEnv are required to drive shed signals")
	}
	return nil
}

func (c *Config) hasLocalDriver() bool {
	for _, u := range c.UPSes {
		if u.Driver != nil {
			return true
		}
	}
	return false
}

// ControlUPS derives the pure-core UPS config (thresholds in the units the core
// expects: runtime seconds, charge percent).
func (c *Config) ControlUPS() map[string]control.UPSConfig {
	out := make(map[string]control.UPSConfig, len(c.UPSes))
	for name, u := range c.UPSes {
		out[name] = control.UPSConfig{
			ShedRuntime:   int(time.Duration(u.ShedRuntime).Seconds()),
			ShedCharge:    u.ShedChargePct,
			RecoverCharge: u.RecoverChargePct,
		}
	}
	return out
}

// ControlLoads derives the pure-core load config (type + governance).
func (c *Config) ControlLoads() map[string]control.LoadConfig {
	out := make(map[string]control.LoadConfig, len(c.Loads))
	for name, l := range c.Loads {
		lt := control.Chassis
		if l.Type == TypeNutServer {
			lt = control.NutServer
		}
		out[name] = control.LoadConfig{Type: lt, GovernedBy: l.GovernedBy}
	}
	return out
}
