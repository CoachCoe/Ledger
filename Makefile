.PHONY: build test lint fuzz security demo

build:
	go build -o bin/ledger ./cmd/ledger

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

security:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

demo:
	bash demos/m0.sh
	bash demos/m1.sh

fuzz:
	go test ./internal/canon -run '^$$' -fuzz FuzzCanonical -fuzztime 60s
