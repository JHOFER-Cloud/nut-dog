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
sudo ./test/integration/shed_test.sh        # shed signal shuts a upsmon secondary
sudo ./test/integration/controller_e2e.sh   # real controller sheds/recovers multiple servers
```

Formatted with `gofumpt`. CI (`.github/workflows/ci.yaml`) runs the unit +
integration + image-smoke tests, and on `main` uses semantic-release to version
and push the multi-arch image to `ghcr.io/jhofer-cloud/nut-dog`.

## Deploy

Deployed via GitOps from the `fleet` repo (`infra/nut-dog/`), pinned to an
always-on control-plane Pi, `hostNetwork`, shipping `dryRun: true` until the live
pull-the-plug drill passes.
