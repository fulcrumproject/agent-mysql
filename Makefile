.PHONY: check build integration core-smoke audit

build:
	go build -o bin/fulcrum-mysql-agent ./cmd/agent

check:
	go test -race ./...
	go vet ./...
	go build -o bin/fulcrum-mysql-agent ./cmd/agent

integration:
	go test -tags=integration -race -v ./internal/dockerapi

core-smoke:
	python3 test/core_smoke.py --core-repo ../core

audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
