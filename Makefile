BIN := bin/coach

.PHONY: all build build-linux test vet fmt check clean

all: vet test build

build:
	go build -o $(BIN) ./cmd/coach

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BIN)-linux-amd64 ./cmd/coach

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: build
	node scripts/check-emby-client.mjs

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 coverage.out
