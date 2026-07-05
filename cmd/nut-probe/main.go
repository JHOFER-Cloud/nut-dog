// Command nut-probe dumps a UPS's NUT variables and the telemetry nut-dog would
// derive from them. It exists to validate a UPS end to end (TLS + login + var
// parse) from a workstation before wiring the UPS into the controller.
//
// Credentials come from the environment so they stay out of shell history:
//
//	set -x NUT_USERNAME (op read op://JHC/.../username)
//	set -x NUT_PASSWORD (op read op://JHC/.../password)
//	go run ./cmd/nut-probe -addr ups-b.hla1.jhofer.lan:3493 -ups nut -tls -insecure
//	set -e NUT_USERNAME NUT_PASSWORD
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/JHOFER-Cloud/nut-dog/internal/nut"
)

func main() {
	addr := flag.String("addr", "localhost:3493", "NUT server host:port")
	ups := flag.String("ups", "", "UPS name (as shown by `upsc -l`)")
	useTLS := flag.Bool("tls", false, "upgrade the connection with STARTTLS")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification (self-signed)")
	login := flag.Bool("login", false, "issue LOGIN <ups> after auth")
	timeout := flag.Duration("timeout", 5*time.Second, "connection timeout")
	flag.Parse()

	if *ups == "" {
		fmt.Fprintln(os.Stderr, "error: -ups is required")
		os.Exit(2)
	}

	opts := nut.Options{
		TLS:                *useTLS,
		InsecureSkipVerify: *insecure,
		Username:           os.Getenv("NUT_USERNAME"),
		Password:           os.Getenv("NUT_PASSWORD"),
		Login:              *login,
	}

	vars, err := nut.Fetch(*addr, *ups, opts, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch %s@%s: %v\n", *ups, *addr, err)
		os.Exit(1)
	}

	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Printf("--- %d vars from %s@%s ---\n", len(vars), *ups, *addr)
	for _, k := range names {
		fmt.Printf("%-28s %s\n", k, vars[k])
	}
	fmt.Printf("\nderived telemetry: %+v\n", nut.TelemetryFromVars(vars))
}
