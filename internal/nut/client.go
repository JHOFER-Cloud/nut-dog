package nut

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Options configures a single Fetch. TLS drives the NUT STARTTLS upgrade
// (UPS-B's UniFi server requires it, with a self-signed cert -> InsecureSkip).
// Username/Password authenticate when the server gates reads (UPS-B does);
// Login additionally issues LOGIN <ups> if a server only elevates on login.
type Options struct {
	TLS                bool
	InsecureSkipVerify bool
	Username           string
	Password           string
	Login              bool
}

// connect dials addr, optionally upgrades to TLS, and authenticates with
// USERNAME/PASSWORD when creds are set. The caller closes conn.
func connect(addr string, opts Options, timeout time.Duration) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if opts.TLS {
		if conn, err = startTLS(conn, addr, opts.InsecureSkipVerify); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	r := bufio.NewReader(conn)
	if opts.Username != "" {
		if err := cmdOK(conn, r, "USERNAME "+opts.Username); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		if err := cmdOK(conn, r, "PASSWORD "+opts.Password); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}
	return conn, r, nil
}

// Fetch connects to a NUT server at addr (host:port), optionally upgrades to
// TLS and authenticates, then returns all variables for ups via LIST VAR.
func Fetch(addr, ups string, opts Options, timeout time.Duration) (map[string]string, error) {
	conn, r, err := connect(addr, opts, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if opts.Login {
		if err := cmdOK(conn, r, "LOGIN "+ups); err != nil {
			return nil, err
		}
		defer func() { _, _ = fmt.Fprintf(conn, "LOGOUT\n") }()
	}

	if _, err := fmt.Fprintf(conn, "LIST VAR %s\n", ups); err != nil {
		return nil, err
	}
	return collectVars(r, ups)
}

// SetVar sets a variable on a UPS via the NUT SET command. nut-dog uses it to
// drive a per-server dummy-ups shed signal (ups.status "OB LB" / "OL"), which
// the server's own upsmon self-shuts on. The authenticated user needs SET
// action rights in upsd.users.
func SetVar(addr, ups, name, value string, opts Options, timeout time.Duration) error {
	conn, r, err := connect(addr, opts, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	return cmdOK(conn, r, fmt.Sprintf("SET VAR %s %s %q", ups, name, value))
}

// startTLS performs the NUT STARTTLS handshake. The pre-upgrade response is
// read raw (one byte at a time) so no plaintext bytes get buffered past the TLS
// boundary.
func startTLS(conn net.Conn, addr string, insecure bool) (net.Conn, error) {
	if _, err := fmt.Fprintf(conn, "STARTTLS\n"); err != nil {
		return nil, err
	}
	line, err := readLineRaw(conn)
	if err != nil {
		return nil, fmt.Errorf("STARTTLS: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return nil, fmt.Errorf("STARTTLS refused: %q", line)
	}
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}
	tc := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: insecure})
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tc, nil
}

// collectVars reads a `BEGIN LIST VAR ... END LIST VAR` block into a map. An ERR
// response (e.g. ACCESS-DENIED) is surfaced as an error.
func collectVars(r *bufio.Reader, ups string) (map[string]string, error) {
	vars := make(map[string]string)
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(line, "ERR "):
			return nil, fmt.Errorf("nut error: %s", strings.TrimPrefix(line, "ERR "))
		case strings.HasPrefix(line, "BEGIN LIST VAR"):
			continue
		case strings.HasPrefix(line, "END LIST VAR"):
			return vars, nil
		case strings.HasPrefix(line, "VAR "):
			name, val, ok := parseVarLine(line, ups)
			if ok {
				vars[name] = val
			}
		}
	}
}

// parseVarLine parses `VAR <ups> <name> "<value>"`; the value is quoted and may
// contain spaces (e.g. ups.status "OL CHRG").
func parseVarLine(line, ups string) (name, val string, ok bool) {
	fields := strings.SplitN(line, " ", 4)
	if len(fields) != 4 || fields[0] != "VAR" || fields[1] != ups {
		return "", "", false
	}
	return fields[2], strings.Trim(fields[3], `"`), true
}

func cmdOK(w io.Writer, r *bufio.Reader, cmd string) error {
	if _, err := fmt.Fprintf(w, "%s\n", cmd); err != nil {
		return err
	}
	line, err := readLine(r)
	if err != nil {
		return err
	}
	verb, _, _ := strings.Cut(cmd, " ")
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("%s failed: %s", verb, line)
	}
	return nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func readLineRaw(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return b.String(), err
		}
		if buf[0] == '\n' {
			return strings.TrimRight(b.String(), "\r"), nil
		}
		b.WriteByte(buf[0])
	}
}
