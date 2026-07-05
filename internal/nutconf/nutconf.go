// Package nutconf renders the NUT server-side config (ups.conf, upsd.conf,
// upsd.users, and one dummy-ups .dev per NUT server) from the same Config the
// controller uses. This is what makes "add a UPS or a server" pure config: a new
// local driver, shed signal, and secondary login are all generated from the
// config blocks — no static templates, nothing hardcoded per host.
package nutconf

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JHOFER-Cloud/nut-dog/internal/config"
)

// Getenv resolves a secret env var (injected for testability).
type Getenv func(string) string

// shedDev is the healthy baseline a shed signal starts from; the controller
// flips ups.status to "OB LB" / back to "OL" at runtime via NUT SET.
const shedDev = `ups.status: OL
ups.mfr: nut-dog
ups.model: shed-signal
battery.charge: 100
battery.runtime: 9999
`

// Render builds all NUT server files as filename -> content.
func Render(cfg *config.Config, getenv Getenv) (map[string]string, error) {
	files := make(map[string]string)

	upsConf, err := renderUpsConf(cfg, getenv)
	if err != nil {
		return nil, err
	}
	files["ups.conf"] = upsConf

	upsdConf, err := renderUpsdConf(cfg)
	if err != nil {
		return nil, err
	}
	files["upsd.conf"] = upsdConf

	users, err := renderUpsdUsers(cfg, getenv)
	if err != nil {
		return nil, err
	}
	files["upsd.users"] = users

	for _, name := range nutServers(cfg) {
		files[config.ShedUpsName(name)+".dev"] = shedDev
	}

	// Debian/Ubuntu NUT ships MODE=none, which disables upsd + drivers. We serve
	// external secondaries, so enable netserver mode.
	files["nut.conf"] = "MODE=netserver\n"
	return files, nil
}

// WriteFiles writes rendered files into dir, tightening perms on the two that
// carry secrets.
func WriteFiles(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "ups.conf" || name == "upsd.users" {
			mode = 0o640
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

func renderUpsConf(cfg *config.Config, getenv Getenv) (string, error) {
	var b strings.Builder
	for _, name := range sortedKeys(cfg.UPSes) {
		u := cfg.UPSes[name]
		if u.Driver == nil {
			continue // remote UPS: nut-dog reads it directly, no local driver
		}
		fmt.Fprintf(&b, "[%s]\n", u.UPSName)
		fmt.Fprintf(&b, "    driver = %s\n", u.Driver.Type)
		fmt.Fprintf(&b, "    port = %s\n", u.Driver.Port)
		for _, k := range sortedKeys(u.Driver.Options) {
			fmt.Fprintf(&b, "    %s = %s\n", k, u.Driver.Options[k])
		}
		for _, k := range sortedKeys(u.Driver.SecretOptions) {
			env := u.Driver.SecretOptions[k]
			val, err := resolveSecret(getenv, env)
			if err != nil {
				return "", fmt.Errorf("ups %q option %q: %w", name, k, err)
			}
			fmt.Fprintf(&b, "    %s = %s\n", k, val)
		}
		b.WriteString("\n")
	}
	for _, name := range nutServers(cfg) {
		shed := config.ShedUpsName(name)
		fmt.Fprintf(&b, "[%s]\n    driver = dummy-ups\n    port = %s.dev\n\n", shed, shed)
	}
	return b.String(), nil
}

func renderUpsdConf(cfg *config.Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.LocalUpsd.Listen)
	if err != nil {
		return "", fmt.Errorf("localUpsd.listen %q: %w", cfg.LocalUpsd.Listen, err)
	}
	return fmt.Sprintf("LISTEN %s %s\n", host, port), nil
}

func renderUpsdUsers(cfg *config.Config, getenv Getenv) (string, error) {
	var b strings.Builder
	if cfg.LocalUpsd.AdminUser != "" {
		pw, err := resolveSecret(getenv, cfg.LocalUpsd.AdminPasswordEnv)
		if err != nil {
			return "", fmt.Errorf("localUpsd admin: %w", err)
		}
		fmt.Fprintf(&b, "[%s]\n    password = %s\n    actions = SET\n    instcmds = all\n\n", cfg.LocalUpsd.AdminUser, pw)
	}
	for _, name := range nutServers(cfg) {
		sec := cfg.Loads[name].Secondary
		pw, err := resolveSecret(getenv, sec.PasswordEnv)
		if err != nil {
			return "", fmt.Errorf("load %q secondary: %w", name, err)
		}
		fmt.Fprintf(&b, "[%s]\n    password = %s\n    upsmon secondary\n\n", sec.User, pw)
	}
	return b.String(), nil
}

// resolveSecret fetches a secret env value and rejects anything that would
// corrupt the generated NUT config — empty, or containing a newline/CR that
// could inject extra directives or user stanzas.
func resolveSecret(getenv Getenv, env string) (string, error) {
	v := getenv(env)
	if v == "" {
		return "", fmt.Errorf("secret env %q is empty", env)
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("secret env %q contains a newline", env)
	}
	return v, nil
}

// nutServers returns nut-server load names, sorted, for deterministic output.
func nutServers(cfg *config.Config) []string {
	var names []string
	for name, l := range cfg.Loads {
		if l.Type == config.TypeNutServer {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
