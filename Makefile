.PHONY: build test lint run

BINARY := uv5r-relay
PKG    := ./cmd/uv5r-relay

build:
	go build -o bin/$(BINARY) $(PKG)

test:
	go test ./...

lint:
	go vet ./...

run: build
	./bin/$(BINARY)
