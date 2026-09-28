.PHONY: all build test fmt lint clean

all: fmt test build

build:
	go build -o bin/ ./cmd/...

test:
	go vet ./...
	go test ./...

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)
	go vet ./...

clean:
	rm -rf bin
