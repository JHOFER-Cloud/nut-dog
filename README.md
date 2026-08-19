# nut-dog

UPS-triggered graceful shutdown + load-shedding for the HLA1 rack. A stateless,
level-triggered controller: every poll it reads both UPSes and the actual power
state of each load, then drives the loads toward what the telemetry implies. No
timers, no persisted state — a restart just re-reads the world and continues.

On a mains failure it sheds the blade chassis (BC1) and the Proxmox host (p1)
early so the always-on networking tier rides the battery through short blips,
then powers everything back when mains returns. See
[JHC-501](https://linear.app/jhofer/issue/JHC-501) for the full design.

## How it works

```
┌──────────────── nut-dog pod ────────────────┐
│  nut-dog   (the controller — decides)        │
│  upsd      (NUT server p1's upsmon talks to) │
│   ├─ snmp-ups  → reads UPS-A (CyberPower RMCARD, the grid sensor)
│   └─ dummy-ups → a "shed-<server>" signal per NUT server
└──────────────────────────────────────────────┘
```

- **UPS-A** (RMCARD, SNMPv3) is the grid sensor and powers **BC1**. **UPS-B**
  (UniFi, NUT over TLS) powers the networking tier + **p1**. Both are polled for
  `battery.runtime` / `battery.charge` / `ups.status`.
- A load **sheds** when any governing UPS is critical (`on battery && runtime ≤
  shedRuntime`) and **recovers** only when all are healthy again (charge deadband
  prevents flapping).
- **BC1**: `racadm chassisaction powerdown` / `powerup` over SSH to the CMC;
  blades self-boot on power-up via their iDRAC power-restore policy.
- **p1 (and any NUT server)**: shed over NUT — the controller flips its
  `shed-<name>` dummy-ups critical and its own `upsmon` shuts it down; woken via
  Wake-on-LAN. Adding a server is a config block, nothing else.

### Sharing a load with another controller

energy-watchdog decides when p1 should be off for solar reasons, but nut-dog owns
the power itself: it is the service that has to work during an outage, and its
mechanisms (the shed signal, WoL) need neither Proxmox nor the cluster.

So energy-watchdog asks, over `powerAPI`:

```
PUT /api/loads/p1/power   {"desired": "on" | "off" | "hold", "reason": "..."}
GET /api/loads/p1/state   -> {"actual": "up" | "down" | "unknown", "ageSeconds": 3}
Authorization: Bearer <token>
```

The `GET` serves back what the last reconcile's probe saw. It exists because our probe
answers a question the caller cannot answer for itself: it reaches the host directly, while
energy-watchdog reads node state from the rest of the `pve` cluster, which reports a node
partitioned from corosync exactly as it reports one that is switched off. Acting on that
confusion shed a running host once. `ageSeconds` is computed here, so the caller needs no
agreement with our clock to judge whether the reading is still worth anything.

A request never outranks a UPS event. `DesiredForLoad` resolves it: a critical
source sheds the load regardless of who wants it on; below that, `off` is honoured
in any state, and `on` only once every source is healthy — that rule is the whole
wake interlock, and it needs no coordination with the caller.

`hold` is a request in its own right, not a withdrawal: it pins the load where it is,
which is what stops nut-dog powering it back on by itself while the caller is
mid-operation. That is deliberately different from *no* request at all, which hands the
load back to the UPS verdict — and a healthy UPS means on. There is no way to return a
load to "no request" by hand; `requestTTL` (default 10h) does it on its own once nobody
has restated, and restarting nut-dog clears everything, since it keeps none of this.

Expiry is not a neutral state: it hands the load back to its UPSes, and healthy UPSes mean
*on*. So a caller that dies eventually gets its load powered up rather than pinned where it
left it — deliberate, since a stranded load is the harder failure to notice. energy-watchdog
restates every tick even when it has lost Prometheus, so only a genuinely dead one trips it.

Requests live in memory only. Callers restate their wish every tick, so nothing
has to survive a restart and no stale file can hold a load down. `startupGrace`
covers the window after a restart where nut-dog hasn't been told yet: for that long
it takes no power-*on* action. Sheds are never held — a restart must never delay an
emergency.

`powerAPI.loads` is an allowlist. The token is authority over those loads only.

## Config

Everything is config-driven — see [`config.example.yaml`](config.example.yaml).
The pod's NUT server config (`ups.conf` / `upsd.users` / shed signals) is
**generated** from it, so adding a UPS or server never touches code or templates.
Secret values come from the environment (referenced by `*Env` fields).

## Develop

```sh
go test ./...                 # unit tests
go build ./cmd/nut-dog        # the controller
go build ./cmd/nut-probe      # dump a UPS's telemetry (validate a NUT/SNMP UPS)

# integration tests (need nut-server + nut-client, run as root):
sudo ./test/integration/shed_test.sh          # shed signal shuts a upsmon secondary
sudo ./test/integration/shed_failover_test.sh # ...and still does with a second, unreachable server
sudo ./test/integration/controller_e2e.sh     # real controller sheds/recovers multiple servers
```

Formatted with `gofumpt`. CI (`.github/workflows/ci.yaml`) runs the unit +
integration + image-smoke tests, and on `main` uses semantic-release to version
and push the multi-arch image to `ghcr.io/jhofer-cloud/nut-dog`.

## Deploy

Deployed via GitOps from the `fleet` repo (`infra/nut-dog/`), pinned to an
always-on control-plane Pi, `hostNetwork`, shipping `dryRun: true` until the live
pull-the-plug drill passes.

## Observability

Serves Prometheus metrics (`nut_dog_*`) and `/healthz` on `:9334`: per-UPS
telemetry freshness (the fail-safe's key blind spot), the controller's decisions
(desired/actual/shed per load), action failures, and the full UPS telemetry it
polls (charge, runtime, load, voltages, temperature). Scrape config and alerts
live in `fleet` (`infra/nut-dog/common/serviceMonitor.yaml`, `infra/monitoring/.../nut-dog-alerts.yaml`).

Grafana dashboard: [`fleet-dashboards` → `sync/K8s/Misc/nut-dog.json`](https://github.com/JHOFER-Cloud/fleet-dashboards/blob/main/sync/K8s/Misc/nut-dog.json).
