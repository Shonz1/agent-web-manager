BINARY  := agent-web-manager
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test check clean dist

## build: compile the self-contained binary into bin/
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) .

## run: build and start the manager on http://127.0.0.1:7788
run: build
	./bin/$(BINARY)

## test: run unit tests
test:
	go test ./...

## check: formatting, vet, and tests
check:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt issues above"; exit 1)
	go vet ./...
	go test ./...

## dist: cross-compile release binaries for macOS and Linux
dist:
	@mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-darwin-arm64 .
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-darwin-amd64 .
	GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-linux-arm64 .
	GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-linux-amd64 .

clean:
	rm -rf bin dist
