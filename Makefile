.PHONY: build run test lint vuln verify secure fuzz

build:
	CGO_ENABLED=0 go build -trimpath -o bin/urvi ./cmd/cli

# Run the TUI directly. Loads .env (if present) so LLM_API_KEY and friends are
# exported for the process. Select an agent with AGENT=<name> (default: coding;
# "personal-assistant" is also available). e.g. make run AGENT=personal-assistant
run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/cli $(AGENT)

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
