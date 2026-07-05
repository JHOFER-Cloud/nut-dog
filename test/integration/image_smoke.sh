#!/usr/bin/env bash
# Smoke test for the built image: the NUT server tools are present and the binary
# parses config + renders the NUT server config inside the image. Doesn't need
# real hardware.
set -euo pipefail
IMG="${1:?usage: image_smoke.sh <image>}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

echo "== NUT server tools present in image =="
docker run --rm --entrypoint sh "$IMG" -c '
	set -e
	command -v upsd
	command -v upsdrvctl
	# NUT drivers live in the driver dir, not PATH.
	for d in snmp-ups dummy-ups; do
		[ -x "/lib/nut/$d" ] || [ -x "/usr/lib/nut/$d" ] || { echo "missing driver: $d"; exit 1; }
	done
'

echo "== nut-dog parses config + renders NUT server config in image =="
docker run --rm \
	-v "$ROOT/test/smoke-config.yaml:/config/config.yaml:ro" \
	-e ADMIN_PW=x -e SNMP_A_AUTH_PW=x -e SNMP_A_PRIV_PW=x -e P1_PW=x \
	--entrypoint sh "$IMG" -c '
		set -e
		nut-dog -config /config/config.yaml -render-nut /tmp/out
		ls /tmp/out
		grep -q "\[ups-a\]"   /tmp/out/ups.conf
		grep -q "\[shed-p1\]" /tmp/out/ups.conf
		grep -q "\[p1\]"      /tmp/out/upsd.users
		test -f /tmp/out/shed-p1.dev
	'
echo "PASS: image smoke test"
