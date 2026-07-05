#!/bin/sh
# Generate NUT's server-side config from config.yaml (+ secret env), start the
# local upsd and drivers, then hand off to the controller as PID 1 for clean
# signals. The NUT config is generated — adding a UPS or server is a config
# change only, never an edit here.
set -eu

mkdir -p /etc/nut /var/run/nut

# Render ups.conf / upsd.conf / upsd.users / shed-*.dev from config + env.
nut-dog -config /config/config.yaml -render-nut /etc/nut

# NUT server side (privileged namespace → run as root). Drivers and upsd both
# daemonize; a failed snmp-ups start is non-fatal — the controller fail-safes on
# missing telemetry and the driver retries.
upsdrvctl -u root start || echo "warning: some NUT drivers failed to start" >&2
upsd -u root

exec nut-dog -config /config/config.yaml
