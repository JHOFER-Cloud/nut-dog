#!/usr/bin/env bash
# Integration test for the p1-shed mechanism, end to end (minus the real
# shutdown): stand up a dummy-ups shed signal on a local upsd, point a upsmon
# SECONDARY at it (as p1 would), then drive the signal critical the way nut-dog
# does (SET VAR ups.status "OB LB FSD") and assert upsmon runs its SHUTDOWNCMD.
#
# Requires: nut-server + nut-client. Run as root. Writes config to /etc/nut (the
# canonical location NUT reads MODE/config from — NUT_CONFPATH is unreliable for
# nut.conf); CI runners are ephemeral so clobbering it is fine.
set -euo pipefail

CONF=/etc/nut
WORK="$(mktemp -d)"
MARKER="$WORK/shed-fired"
mkdir -p /run/nut "$CONF"

# Ephemeral throwaway creds (generated, never committed).
ADMINPW="$(openssl rand -hex 16)"
P1PW="$(openssl rand -hex 16)"

cleanup() {
	upsmon -c stop 2>/dev/null || true
	upsd -c stop 2>/dev/null || true
	upsdrvctl -u root stop 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# Debian/Ubuntu NUT ships MODE=none (everything disabled); enable it.
printf 'MODE=netserver\n' >"$CONF/nut.conf"

cat >"$CONF/ups.conf" <<'EOF'
[shed-p1]
    driver = dummy-ups
    port = shed-p1.dev
EOF

cat >"$CONF/shed-p1.dev" <<'EOF'
ups.status: OL
ups.mfr: nut-dog
ups.model: shed-signal
battery.charge: 100
battery.runtime: 9999
EOF

cat >"$CONF/upsd.conf" <<'EOF'
LISTEN 127.0.0.1 3493
EOF

cat >"$CONF/upsd.users" <<EOF
[admin]
    password = $ADMINPW
    actions = SET
    instcmds = all
[p1]
    password = $P1PW
    upsmon secondary
EOF

# The stand-in for p1: a secondary whose "shutdown" just drops a marker file.
cat >"$CONF/upsmon.conf" <<EOF
MONITOR shed-p1@127.0.0.1 1 p1 $P1PW secondary
MINSUPPLIES 1
SHUTDOWNCMD "touch $MARKER"
POLLFREQ 1
POLLFREQALERT 1
EOF

chmod 640 "$CONF/upsd.users" "$CONF/upsmon.conf"

echo "== start dummy-ups + upsd =="
upsdrvctl -u root start
upsd -u root
sleep 1
upsc shed-p1@127.0.0.1 ups.status

echo "== start p1's upsmon secondary =="
upsmon -u root
sleep 2
if [ -f "$MARKER" ]; then
	echo "FAIL: shutdown fired before we shed"
	exit 1
fi

echo "== drive the shed signal critical (as nut-dog's SetVar does) =="
upsrw -s ups.status="OB LB FSD" -u admin -p "$ADMINPW" shed-p1@127.0.0.1
upsc shed-p1@127.0.0.1 ups.status

echo "== await SHUTDOWNCMD =="
for _ in $(seq 1 20); do
	[ -f "$MARKER" ] && break
	sleep 1
done

if [ -f "$MARKER" ]; then
	echo "PASS: shed signal triggered the secondary's SHUTDOWNCMD"
else
	echo "FAIL: upsmon did not run SHUTDOWNCMD within timeout"
	exit 1
fi
