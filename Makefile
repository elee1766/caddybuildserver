.PHONY: build clean api worker janitor all

all: build

build: api worker janitor

api:
	go build -o bin/api ./cmd/api

worker:
	go build -o bin/worker ./cmd/worker

janitor:
	go build -o bin/janitor ./cmd/janitor

clean:
	@echo "cleaning binaries..."
	@rm -f bin/api bin/worker bin/janitor

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...
