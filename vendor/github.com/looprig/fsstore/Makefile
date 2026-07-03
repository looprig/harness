.PHONY: test fmt-check vet check

test:
	GOWORK=off go test -race ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

vet:
	GOWORK=off go vet ./...

check: fmt-check vet test
