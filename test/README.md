# Verification

## Fast checks

`make check` runs race-enabled unit tests, `go vet` and the executable build.

## Docker/MySQL integration

```sh
go test -tags=integration -race -v ./internal/dockerapi
```

Requires native Linux access to `/var/run/docker.sock`, Docker CLI, and internet access for the pinned image unless already pulled. Uses loopback-only published ports and unique service IDs, bounded CPU/RAM, temporary owner-only bootstrap state, and dedicated named volumes/networks.

`TestDockerLifecycle` covers port-conflict recovery, SQL privileges, isolation between two services, persistence across restart and cold resize, metrics, ownership checks and deletion. It checks Docker metadata and server logs for plaintext credentials. It does not restart the Docker daemon or host.

Cleanup removes only the resources created by the test, including test data. A hard-killed test process may leave its uniquely labelled resources; inspect ownership before manually cleaning them. Never use `docker system prune` as test cleanup.

## Real Core smoke test

```sh
python3 test/core_smoke.py --core-repo ../core
```

Requires Python 3, Go, Docker and a compatible Fulcrum Core checkout. The script builds both binaries and runs Core against a disposable PostgreSQL database on loopback, using token authentication and Core's empty-database bootstrap token. It does not use an existing Core deployment or production credentials.

It registers the example schemas and test resources through the API, then checks failed-create recovery, parameterless retry with a vault secret, lifecycle guards, cold update, metric delivery and deletion with port/secret cleanup.

Cleanup matches the fresh test agent ID and managed label, never a global prefix alone. Private logs are retained under the printed temporary evidence directory; secret state and binaries are removed. Treat even those test logs as private and sanitize before sharing. The external SQL client uses RSA public-key retrieval only inside the controlled loopback test environment; do not copy it as a recommendation for untrusted networks.

`make audit` runs a pinned govulncheck tool against the current vulnerability database. CI runs unit, static, build, vulnerability and Docker integration checks. The Core smoke test is explicit because it needs a separately selected Core revision; record that revision with its results.
