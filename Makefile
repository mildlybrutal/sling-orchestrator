# Common tasks. Everything here is plain `go`; the Makefile only saves typing.
.PHONY: build test test-integration e2e demo vet fmt clean

BIN := bin/sling

build:                ## compile the sling binary into bin/
	go build -o $(BIN) ./cmd/sling

test:                 ## unit tests with the race detector
	go test -race ./...

test-integration:     ## tests against a real Caddy binary (one package at a time: they share ports 80/2019): make test-integration CADDY=/path/to/caddy
	CADDY=$(CADDY) go test -p 1 -tags integration ./...

e2e: build            ## deploy every demo case with Docker (needs ../sling)
	./scripts/run-cases.sh ../sling

demo:                 ## record the README demo (needs ../sling with the fails-later tag) into docs/demo.svg
	python3 scripts/record.py /tmp/sling-demo.cast -- ./scripts/demo.sh ../sling
	python3 scripts/cast2svg.py /tmp/sling-demo.cast docs/demo.svg --rows 28

vet:
	go vet ./... && go vet -tags integration ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -rf bin
