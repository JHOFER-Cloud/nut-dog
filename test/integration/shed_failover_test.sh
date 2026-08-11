#!/usr/bin/env bash
# Integration test for the MULTI-SERVER upsmon topology p1 actually runs.
#
# nut-dog is hostNetwork and floats between the two control-plane rpis, so p1's
# upsmon monitors BOTH — and one of them is always unreachable. That phantom
# target is not inert: upsmon assumes a UPS it has never heard from is OL
# (see is_ups_critical(); commstate starts at -1, so the "assuming dead" branch,
# which needs commstate==0 AND linestate==0, never fires). It therefore keeps
# contributing its powervalue.
#
# With MINSUPPLIES 1 that phantom alone satisfies the minimum, so a real FSD on
# the live server leaves val_ol == 1 and upsmon never shuts down — silently. That
# is a shed that does nothing during a power event, so both directions are pinned
# here: MINSUPPLIES 1 must FAIL to shut down, MINSUPPLIES 2 must succeed.
#
# Requires: nut-server + nut-client. Run as root. See shed_test.sh for why config
# goes to the canonical /etc/nut.
set -euo pipefail

CONF=/etc/nut
WORK="$(mktemp -d)"
MARKER="$WORK/shed-fired"
LIVE=127.0.0.1:3493
PHANTOM=127.0.0.1:3494 # nothing ever listens here: the rpi not running nut-dog
mkdir -p /run/nut "$CONF"

ADMINPW="$(openssl rand -hex 16)"
P1PW="$(openssl rand -hex 16)"

cleanup() {
	upsmon -c stop 2>/dev/null || true
	upsd -c stop 2>/dev/null || true
	upsdrvctl -u root stop 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

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
chmod 640 "$CONF/upsd.users"

# write_upsmon_conf <minsupplies>
write_upsmon_conf() {
	cat >"$CONF/upsmon.conf" <<EOF
MONITOR shed-p1@$LIVE 1 p1 $P1PW secondary
MONITOR shed-p1@$PHANTOM 1 p1 $P1PW secondary
MINSUPPLIES $1
SHUTDOWNCMD "touch $MARKER"
POLLFREQ 1
POLLFREQALERT 1
EOF
	chmod 640 "$CONF/upsmon.conf"
}

set_status() {
	upsrw -s ups.status="$1" -u admin -p "$ADMINPW" "shed-p1@$LIVE" >/dev/null
}

# await_marker <seconds>; returns 0 if SHUTDOWNCMD ran
await_marker() {
	for _ in $(seq 1 "$1"); do
		[ -f "$MARKER" ] && return 0
		sleep 1
	done
	[ -f "$MARKER" ]
}

echo "== start dummy-ups + upsd =="
upsdrvctl -u root start
upsd -u root
sleep 1
upsc "shed-p1@$LIVE" ups.status

# --- Phase 1: the regression. MINSUPPLIES 1 must NOT shut down. ---------------
echo
echo "== phase 1: MINSUPPLIES 1 (must NOT shed: the phantom masks the FSD) =="
rm -f "$MARKER"
set_status "OL"
write_upsmon_conf 1
upsmon -u root
sleep 3

# Control: without this, a upsmon that died at startup would "pass" phase 1 by
# never firing, and phase 2 would be the only real test.
if ! pgrep -x upsmon >/dev/null; then
	echo "FAIL: upsmon is not running - phase 1 would pass vacuously"
	exit 1
fi
if [ -f "$MARKER" ]; then
	echo "FAIL: shutdown fired before we shed"
	exit 1
fi

set_status "OB LB FSD"
if await_marker 12; then
	echo "UNEXPECTED: MINSUPPLIES 1 shut down despite the phantom supply."
	echo "upsmon's accounting changed - re-check is_ups_critical() and MINSUPPLIES 2 below."
	exit 1
fi
echo "PASS: MINSUPPLIES 1 did not shed (the bug this guards against)"
upsmon -c stop
sleep 1

# --- Phase 2: the fix. MINSUPPLIES 2 must shut down. --------------------------
echo
echo "== phase 2: MINSUPPLIES 2 (must shed on FSD) =="
rm -f "$MARKER"
set_status "OL" # back to healthy, or upsmon sheds the instant it starts
write_upsmon_conf 2
upsmon -u root
sleep 3
if [ -f "$MARKER" ]; then
	echo "FAIL: shutdown fired while the signal was still OL"
	exit 1
fi

set_status "OB LB FSD"
if ! await_marker 20; then
	echo "FAIL: MINSUPPLIES 2 did not run SHUTDOWNCMD within timeout"
	exit 1
fi
echo "PASS: MINSUPPLIES 2 shed on FSD with a phantom second server"
