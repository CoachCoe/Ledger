.PHONY: build test lint demo security fuzz check

build:
	go build -o bin/ledger ./cmd/ledger

test:
	go build ./...
	go test -race -p 1 ./...

lint:
	go vet ./...
	golangci-lint run

demo:
	bash demos/m0.sh
	bash demos/m1.sh
	bash demos/m2.sh
	bash demos/m3.sh
	bash demos/m4.sh
	bash demos/m5.sh
	bash demos/m6.sh
	bash demos/governance.sh

security:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

fuzz:
	go test ./internal/canon -run '^$$' -fuzz FuzzCanonical -fuzztime 60s

check: lint test demo security
