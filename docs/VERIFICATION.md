# Verification baseline

Revision: `e8e902f`. [CI run](https://github.com/fulcrumproject/agent-mysql/actions/runs/42207597499).

## Environment

- Fedora 44, amd64; Go 1.27.3; Docker Engine 29.9.3.
- MySQL 9.7.4, pinned by digest in `internal/config/config.go`.
- Core `3212fc7d41422be92b53997770d3311fcab8bf36`, with a disposable PostgreSQL 17 database and loopback endpoints.

## Results

| Check | Result |
| --- | --- |
| `make check` | Unit tests with race detector, vet and build passed |
| `make integration` | Passed with real Docker/MySQL and two simultaneous services |
| `python3 test/core_smoke.py --core-repo ../core` | Passed locally, including registration of the example schemas |
| `make audit` | No reachable vulnerabilities or advisories in imported packages |

The audit reported one module-only advisory, GO-2026-5932, for the unimported `golang.org/x/crypto/openpgp` package. Results above apply to this revision and scan, not future dependency or vulnerability-database updates.

See [test/README.md](../test/README.md) for coverage, prerequisites and cleanup.

## Not tested

- Host or Docker daemon reboot: only server-process restart and the `unless-stopped` configuration were tested.
- ARM64, other Docker versions, alternative filesystems and SELinux-enforcing bind-mount policies.
- Disk-full/disk-loss behavior, TLS identity verification and performance under load.
