# Fulcrum MySQL Agent

A Fulcrum agent for dedicated MySQL instances on a **local Linux Docker Engine**. Each service owns a container, a persistent data volume, a bridge network and protected bootstrap files.

The architecture follows [agent-proxmox-fae](https://github.com/fulcrumproject/agent-proxmox-fae): Go bootstrap, provider configuration from Core, typed lifecycle handlers, a backend interface, and `agent-lib-go/stdagent` for polling, health and metrics. There is no extra HTTP service or job scheduler.

## Scope and limitations

- One agent per Docker host; local Unix socket only. Run the agent **natively on the same host**, not in another container or mount namespace.
- Official MySQL **9.7.4**, pinned by multi-platform manifest digest in `internal/config/config.go`. No customer-selected images or automatic upgrades.
- CPU limits in cores; memory in MiB, minimum 1024. Provider maximums are checked again by the agent. Limits are not capacity reservations; the provider must avoid overcommitting its host.
- Persistent disk **without quotas, backups or HA**. A full/lost host disk affects its services. Persistence is not a backup.
- Private host interface and a stable port per service. Wildcard/public bind addresses are rejected. Loopback is allowed for development; the provider is responsible for DNS, routing and firewall rules. Do not publish these endpoints through a public NAT.
- TLS is not required or forcibly disabled. No certificate management or server-identity guarantee. Queries/results can travel unencrypted: use a trusted network or independently secured tunnel for sensitive data.
- One customer database and user, with privileges only on that database. Database names, users and passwords are immutable through the agent. Do not change credentials out of band; that invalidates the managed connection checks.
- No remote Docker, hot resize, credential rotation, database rename or managed restore.

## Build and run

Prerequisites: Go 1.27.3+, Linux, Docker Engine with CPU/memory/swap/PID limit support, and Fulcrum Core with its vault configured. Tested with Docker 29.9.3. The agent's Docker access is effectively host-root access: treat its Core token and configuration as privileged.

```sh
go build -o bin/fulcrum-mysql-agent ./cmd/agent
# Optional: pre-pull to avoid using the job deadline for an image download.
docker pull mysql:9.7.4@sha256:df90258da46f6e7ff8574ff84f17618aba0765cac749928299c69fd4e7cb8ad8

export MYSQL_AGENT_FULCRUM_API_URL=https://core.example.org
export MYSQL_AGENT_FULCRUM_API_TOKEN=YOUR_AGENT_TOKEN
./bin/fulcrum-mysql-agent
```

Alternatively copy `examples/config.json` to an owner-only file and pass `-config /path/to/config.json`. Environment variables use the `MYSQL_AGENT_` prefix; polling settings are `JOB_POLL_INTERVAL` and `METRIC_REPORT_INTERVAL` (for example `5s` and `30s`). In JSON files, those two `time.Duration` fields use integer nanoseconds, as in the checked-in example; omit them to retain defaults. Keep Core transport authenticated and encrypted when it is not a loopback test deployment; optional MySQL TLS does **not** mean Core secrets should cross an unprotected network.

A systemd example is in `examples/fulcrum-mysql-agent.service`. Bootstrap files containing the agent token must be readable only by the service account. The native process must be able to access Docker's data-root filesystem for disk metrics and its configured state directory.

## Register with Core

The JSON files under `examples/` are API request examples, not a separate schema dialect.

1. Create a ServiceType using `examples/service-type.json` (`POST /api/v1/service-types`).
2. Substitute its ID into `examples/agent-type.json` and create the AgentType. No infrastructure-type association is needed for this standalone agent.
3. Create a **host-specific ServicePoolSet** for the provider. Create the integer list pool in `examples/port-pool.json`, with type `mysql.port`, in that set.
4. Add available ports through `POST /api/v1/service-pool-values`, for example `{"servicePoolId":"POOL_ID","name":"33061","value":33061}`. Reserve those ports for this agent; a conflicting listener causes an explicit, recoverable failure.
5. Substitute IDs and private networking into `examples/agent.json` and create the Agent. Create an agent-role token via `POST /api/v1/tokens` with its ID as `scopeId`; use the returned token in the local bootstrap configuration.
6. Register the five definitions in `examples/metrics.json` through `POST /api/v1/metric-types`.
7. Create a service group for the consumer, then use `examples/service.json` with the real IDs and a strong password. **Do not supply a port**: Core allocates it from the pool. Keep the password in the caller's own secret store; consumers do not retrieve it from the agent.

The provider's `configuration.docker` contains:

| Field | Default / requirement |
| --- | --- |
| `bindIp` | Required private interface IP (not a hostname) |
| `host` | Required private DNS hostname/IP advertised to clients |
| `socketPath` | `/var/run/docker.sock`; `DOCKER_HOST` and Docker contexts are intentionally ignored |
| `stateDir` | `/var/lib/fulcrum-agent-mysql`; absolute, private directory on this host |
| `maxCpu` | 64; configurable from 1 to 64 |
| `maxMemory` | 262144 MiB; configurable from 1024 to 262144 |
| `operationTimeout` | 180 seconds |
| `stopTimeout` | 60 seconds; no greater than operationTimeout |

Align Core's job timeout with operation/polling time and queue delays; enable Core's job maintenance when timeout recovery is required. This agent does not replace the shared library's job acknowledgement or retry behavior. Provider configuration is loaded on connection; restart the agent to change it. Do not change `stateDir`, `host` or `bindIp` for existing services as an implicit migration.

Each instance uses its own bridge network. Configure Docker's address pools for the intended number of instances. The default local volume driver and Docker data-root filesystem are used; external volume drivers are not part of this version.

## Lifecycle

| Operation | Allowed state | Result |
| --- | --- | --- |
| create | New | Started, only after customer SQL succeeds and the published port is reachable |
| stop | New, Started, Stopped | Stopped; data retained, graceful shutdown without forced kill |
| start | Stopped | Started, SQL-ready |
| update CPU/RAM | Stopped | Stopped; same volume, credentials and endpoint |
| delete | Stopped | Deleted; container, data volume, network and local secret files destroyed |

Use `PATCH /api/v1/services/ID` with partial `properties` for resource changes. For actions, use `POST /api/v1/services/ID/start`, `/stop`, `/delete`, or `/create` to retry an unsuccessful creation. Core actions have no parameters; the agent resolves the existing persistent vault secret when retrying create.

A failed job does not trigger destructive rollback. Names and labels derive from Core service IDs, allowing explicit retry without duplication. After a failed/partial create, `stop` also works from New so the service can be deliberately deleted. Missing secret state or ownership/configuration conflicts fail closed; the agent does not adopt foreign resources or initialize an existing volume with new credentials.

Docker's `unless-stopped` policy restarts an exited server and normally restores running services after host/daemon restart, without Core availability. Explicitly stopped instances remain stopped. This is not failover, and unhealthy SQL alone does not trigger automatic destructive repair.

Successful jobs return only non-secret metadata in `agentInstanceData`: container ID, volume, host, port, database and username. `agentInstanceId` is the stable Core service ID, not a transient container identifier.

## Secrets and storage

Customer passwords are persistent vault properties. Root passwords are generated separately per service. Owner-only files under `stateDir/AGENT_ID/SERVICE_ID/secrets` are mounted read-only for bootstrap; Docker environment configuration contains file paths, never password values. Root is restricted to local socket administration. Bootstrap SQL and client option-file escaping are tested with quoted/backslash-containing passwords.

The local state directory is essential operational state: **do not delete it while retaining the database volume**. Store it on persistent storage with mode 0700; secret files are 0600. These files are not encrypted at rest by the agent. A host administrator or anyone controlling Docker can access them. A per-agent process lock prevents two processes from concurrently owning the same state directory.

MySQL uses half the configured memory for the InnoDB buffer pool and a fixed 50-connection limit, leaving headroom for other allocations. Cold resize updates both the cgroup limits and the MySQL configuration. OOM/disk pressure is still possible; host capacity management remains the provider's responsibility.

## Metrics

- `mysql.cpu.usage`: CPU usage relative to the configured CPU limit (ratio).
- `mysql.memory.usage`: cgroup memory usage, including accounted cache, relative to its limit (ratio).
- `mysql.uptime`: seconds since the current container process started.
- `mysql.available`: local customer SQL probe, 0 or 1; not an end-to-end client network check.
- `mysql.host.disk.free`: bytes available on the **shared Docker data-root filesystem**, repeated per service. Never interpret this as a service quota.

Agent health checks Docker connectivity and required resource-limit support. One failing database does not disable management of the others.

## Development and verification

```sh
make check
make audit
make integration
python3 test/core_smoke.py --core-repo ../core
```

See `test/README.md` for isolation, cleanup and prerequisites, `docs/CONTRACT.md` for the approved scope, and `docs/VERIFICATION.md` for recorded evidence and limitations. Tests do not reboot the host or restart a shared Docker daemon.

## License

Apache License 2.0. MySQL and the Docker image retain their own licenses; they are not relicensed by this repository.
