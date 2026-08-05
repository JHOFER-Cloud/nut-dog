#!/usr/bin/env bash
# End-to-end with the REAL controller and MULTIPLE servers: run nut-dog against a
# simulated upsd where UPS-A is a drivable dummy and p1 + p2 each have a shed
# signal. Drive UPS-A critical and prove the controller sheds BOTH servers, then
# recover it and prove it releases BOTH. Complements shed_test.sh (which proves a
# shed signal actually shuts a upsmon secondary); this proves the poll -> Decide
# -> shed loop scales to several servers.
#
# Requires: go, nut-server, nut-client. Run as root. NUT config goes in /etc/nut
# (canonical); nut-dog's own config is a separate file passed via -config.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CONF=/etc/nut
WORK="$(mktemp -d)"
BIN="$WORK/nut-dog"
DOG_PID=""
mkdir -p /run/nut "$CONF"

# Ephemeral throwaway admin password (generated, never committed).
ADMINPW="$(openssl rand -hex 16)"

cleanup() {
	# Reap nut-dog before upsd: SIGTERM is async, so its in-flight poll would
	# otherwise race the dying server and log spurious connection-refused errors.
	if [ -n "$DOG_PID" ]; then
		kill "$DOG_PID" 2>/dev/null || true
		wait "$DOG_PID" 2>/dev/null || true
	fi
	upsd -c stop 2>/dev/null || true
	upsdrvctl -u root stop 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "== build controller =="
(cd "$ROOT" && go build -o "$BIN" ./cmd/nut-dog)

# Debian/Ubuntu NUT ships MODE=none (everything disabled); enable it.
printf 'MODE=netserver\n' >"$CONF/nut.conf"

# upsd: a drivable UPS-A + one shed signal per server.
cat >"$CONF/ups.conf" <<'EOF'
[ups-a]
    driver = dummy-ups
    port = ups-a.dev
[shed-p1]
    driver = dummy-ups
    port = shed-p1.dev
[shed-p2]
    driver = dummy-ups
    port = shed-p2.dev
EOF
printf 'ups.status: OL\nbattery.charge: 100\nbattery.runtime: 900\n' >"$CONF/ups-a.dev"
printf 'ups.status: OL\nbattery.charge: 100\nbattery.runtime: 9999\n' >"$CONF/shed-p1.dev"
printf 'ups.status: OL\nbattery.charge: 100\nbattery.runtime: 9999\n' >"$CONF/shed-p2.dev"

cat >"$CONF/upsd.conf" <<'EOF'
LISTEN 127.0.0.1 3493
EOF
cat >"$CONF/upsd.users" <<EOF
[admin]
    password = $ADMINPW
    actions = SET
    instcmds = all
EOF
chmod 640 "$CONF/upsd.users"

# UPS-A governs both p1 and p2. probe points at upsd (always up) so recovery
# takes the release path without a WoL to loopback.
cat >"$WORK/config.yaml" <<'EOF'
pollInterval: 1s
dryRun: false
localUpsd: { listen: "127.0.0.1:3493", adminUser: admin, adminPasswordEnv: ADMIN_PW }
upses:
  ups-a: { host: 127.0.0.1:3493, upsName: ups-a, shedRuntime: 5m, recoverChargePct: 5 }
loads:
  p1: { type: nut-server, governedBy: [ups-a], wake: { mac: "00:11:22:33:44:55", broadcast: "127.0.0.1:9" }, probe: { host: 127.0.0.1:3493 }, secondary: { user: p1, passwordEnv: SEC } }
  p2: { type: nut-server, governedBy: [ups-a], wake: { mac: "00:11:22:33:44:66", broadcast: "127.0.0.1:9" }, probe: { host: 127.0.0.1:3493 }, secondary: { user: p2, passwordEnv: SEC } }
EOF

wait_status() { # $1=ups $2=expected
	for _ in $(seq 1 15); do
		[ "$(upsc "$1@127.0.0.1" ups.status 2>/dev/null || true)" = "$2" ] && return 0
		sleep 1
	done
	echo "FAIL: $1 ups.status = '$(upsc "$1@127.0.0.1" ups.status 2>/dev/null || true)', want '$2'"
	return 1
}

echo "== start NUT + controller =="
upsdrvctl -u root start
upsd -u root
sleep 1
ADMIN_PW="$ADMINPW" "$BIN" -config "$WORK/config.yaml" &
DOG_PID=$!
sleep 2

echo "== drive UPS-A critical (on battery, runtime under the 5m floor) =="
upsrw -s battery.runtime=60 -u admin -p "$ADMINPW" ups-a@127.0.0.1
upsrw -s ups.status="OB DISCHRG" -u admin -p "$ADMINPW" ups-a@127.0.0.1

wait_status shed-p1 "OB LB FSD"
wait_status shed-p2 "OB LB FSD"
echo "PASS: controller shed BOTH p1 and p2"

echo "== recover UPS-A =="
upsrw -s battery.charge=100 -u admin -p "$ADMINPW" ups-a@127.0.0.1
upsrw -s ups.status="OL" -u admin -p "$ADMINPW" ups-a@127.0.0.1

wait_status shed-p1 "OL"
wait_status shed-p2 "OL"
echo "PASS: controller released BOTH on recovery"
