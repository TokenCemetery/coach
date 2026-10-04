BIN := bin/coach

.PHONY: all build build-linux test vet lint check clean

all: lint test build

build:
	go build -o $(BIN) ./cmd/coach

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BIN)-linux-amd64 ./cmd/coach

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

check: build
	node scripts/check-emby-client.mjs

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 coverage.out
