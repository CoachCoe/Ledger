.PHONY: build test lint demo security fuzz check

build:
	go build -o bin/ledger ./cmd/ledger

test:
	go build ./...
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

demo:
	bash demos/m0.sh
	bash demos/m1.sh

security:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

fuzz:
	go test ./internal/canon -run '^$$' -fuzz FuzzCanonical -fuzztime 60s

check: lint test demo security
