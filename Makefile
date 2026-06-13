.PHONY: build test lint vuln verify secure fuzz

build:
	CGO_ENABLED=0 go build -trimpath -o bin/urvi ./cmd/urvi

test:
	go test -race ./...

lint:
	go vet ./...
	go tool staticcheck ./...
	go tool gosec ./...

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
