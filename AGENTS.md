# Agent MySQL

Read `docs/CONTRACT.md` before changing behavior. Keep bootstrap in `cmd/agent`, configuration in `internal/config`, lifecycle handlers in `internal/mysqlagent`, and the Docker adapter in `internal/dockerapi`. Reuse `agent-lib-go/stdagent` for polling, health and metrics.

- Local Linux Docker socket only; no remote runtime or arbitrary images.
- Preserve data on errors. Never prune Docker globally. Only delete resources whose agent/service labels match, after confirming the container is stopped.
- Keep secrets out of logs, Docker configuration environment values and public runtime metadata.
- Comment non-obvious constraints and decisions, not what the code already says. JSON fields use camelCase.
- Checks: `go test -race ./...`, `go vet ./...`, `go build ./cmd/agent`.
- Real runtime verification: `go test -tags=integration -v ./internal/dockerapi`. It creates only uniquely named test resources on local Docker.
- Core verification: see `test/README.md`. Never use production credentials or delete another run's resources.
- No modifications to sibling repositories or publishing without authorization.
