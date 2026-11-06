# Verification

## Fast checks

`go test -race ./...` covers lifecycle/ownership guards, secret resolution and redaction, partial resource updates, configuration validation, state durability/permissions, immutable credentials and metric normalization. `go vet ./...` and `go build ./cmd/agent` check static correctness and the executable.

## Docker/MySQL integration

```sh
go test -tags=integration -race -v ./internal/dockerapi
```

Requires native Linux access to `/var/run/docker.sock`, Docker CLI, and internet access for the pinned image unless already pulled. Uses loopback-only published ports and unique service IDs, bounded CPU/RAM, temporary owner-only bootstrap state, and dedicated named volumes/networks.

The test exercises a port conflict followed by explicit retry, real TCP SQL connections with quoted passwords, forbidden global/system-database access, remote root denial, stop/cold resize/start with persisted rows, agent restart, server-initiated exit and automatic restart, statistics, two live services with separate data/credentials, foreign-volume protection, and repeatable deletion. It checks Docker inspect and server logs for plaintext credentials. It never restarts the shared Docker daemon or host.

Cleanup removes only the resources created by the test, including test data. A hard-killed test process may leave its uniquely labelled resources; inspect ownership before manually cleaning them. Never use `docker system prune` as test cleanup.

## Real Core smoke test

```sh
python3 test/core_smoke.py --core-repo ../core
```

Requires Python 3, Go, Docker and a compatible Fulcrum Core checkout. The script builds both binaries into a private temporary directory, launches a uniquely named PostgreSQL container with its own ephemeral database and loopback port, and launches Core with token-only authentication. It uses the bootstrap token supplied by Core's empty-database seed, **not production credentials**. No existing Core, database, agent, source file or runtime is modified.

It registers the checked-in schemas, provider/consumer, port pool, agent/token and metric types using the real API. It observes a failed create, an explicit parameterless retry resolving the vault secret, SQL-ready completion, forbidden hot update/delete, immutable password, stop/update/start, metric delivery and deletion. It verifies both port release and persistent vault-secret cleanup.

All resources are isolated. Cleanup matches the fresh test agent ID and managed label, never a global prefix alone. Private logs are retained under the printed temporary evidence directory; secret state and binaries are removed. Treat even those test logs as private and sanitize before sharing. The external SQL client uses RSA public-key retrieval only inside the controlled loopback test environment; do not copy it as a recommendation for untrusted networks.

`make audit` runs a pinned govulncheck tool against the current vulnerability database. CI runs unit, static, build, vulnerability and Docker integration checks. The Core smoke test is explicit because it needs a separately selected Core revision; record that revision with its results.
