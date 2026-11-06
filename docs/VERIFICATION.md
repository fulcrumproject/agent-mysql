# Initial implementation verification

Results below concern the source tree accompanying this document. The committed revision is rechecked before delivery; use Git history to identify that revision. No production data or shared daemon restart was used.

## Environment

- Native Linux (Fedora 44), amd64.
- Go 1.27.3; Docker Engine 29.9.3.
- MySQL 9.7.4, pinned manifest digest in `internal/config/config.go`.
- Real Core checkout: `3212fc7d41422be92b53997770d3311fcab8bf36`.
- Core smoke: isolated, ephemeral PostgreSQL 17 container; token authentication and vault; loopback interfaces; fresh API-created participants, agent, service group, port pool and metric types.
- Temporary SQL rows and credentials only. Docker resources removed after verification; private logs retained outside the repository.

## Commands

| Check | Result |
| --- | --- |
| `make check` | Passed: race-enabled unit tests, vet and executable build |
| `make integration` | Passed with real Docker/MySQL, including two simultaneously running services |
| `python3 test/core_smoke.py --core-repo ../core` | Passed against the real Core API and a newly built agent binary |
| `make audit` | Passed: zero reachable vulnerabilities and zero advisories in imported packages |
| JSON examples | Parse successfully; service/agent/metric/pool definitions also accepted by real Core |

The vulnerability scan is point-in-time, not a security certification. It reports one module-only advisory, GO-2026-5932, for the **unimported** `golang.org/x/crypto/openpgp` package. No production package in this agent imports that package. Go and transitive networking/Chi dependencies were updated to address the initially detected reachable/imported vulnerabilities; the shared Fulcrum repositories were not modified.

## Contract criteria

| Criterion | Evidence / verdict |
| --- | --- |
| 1. SQL-ready create | Satisfied: customer SQL probe, reachable published port, real host-network SQL write/read, and Core Started state |
| 2. Stop/start preserves data | Satisfied: inserted row survives stop, backend-process replacement and start; endpoint unchanged |
| 3. Cold CPU/RAM update | Satisfied: running update rejected, stopped limits inspected, MySQL buffer pool checked after restart; Core properties updated only on successful job |
| 4. Explicit destructive delete | Satisfied: running delete rejected; stopped deletion removes owned resources and local secrets; Core releases the port and vault secret |
| 5. Restart policy | Satisfied for configured `unless-stopped` policy, server-initiated exit/restart and preservation of manually stopped state across agent restart. Host/daemon reboot was not exercised |
| 6. Idempotence/ownership | Satisfied: real port-binding failure followed by explicit retry reuses container and root credentials; repeat create/update/delete; foreign volume preserved; duplicate process lock tested |
| 7. Preserve on errors | Satisfied: failed bind retains resources; real Core job Failed/New state can be explicitly retried; handlers never invoke destructive rollback |
| 8. Secret handling | Satisfied: persistent vault references in public API; no passwords in Docker inspect, server logs or agent logs; private files and escaping tested; remote root rejected |
| 9. Health/metrics | Satisfied: runtime health registered through stdagent; real CPU/memory/uptime/SQL/disk statistics and metric entries delivered to Core |
| 10. Documentation/definitions/license | Satisfied: checked-in registration JSON, configuration/systemd examples, README, reproducible checks and Apache 2.0 license |

Isolation was exercised with two services using the same database/user names but different credentials, containers and volumes. Cross-instance authentication fails; rows remain separate. Global/system-database access and wildcard-schema privilege escape attempts are rejected.

## Limits of this verification

- The host and shared Docker daemon were **not rebooted**. Policy configuration and server process restart were tested instead.
- No backup/restore, disk-loss, disk-full, HA, TLS identity or performance guarantees are asserted; these are outside the approved scope.
- ARM64 is present in the pinned image manifest but was not executed. Other Docker versions, alternative filesystems and SELinux-enforcing bind-mount policies were not exercised.
- The CI workflow has not run remotely until the initial commit is pushed. Creating the public repository does not publish these files.
- A retry after an ambiguous Core acknowledgement still uses the existing Core timeout/explicit-action model. No new durable job queue or automatic replay system was added.

Risk classes: secrets/authentication, infrastructure and new dependencies, persistent data, and irreversible deletion. Use only the documented private-network deployment model and ensure the provider supplies host-capacity monitoring and any required external data protection.
