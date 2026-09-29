.PHONY: all build test test-surrealdb test-embedded fmt lint clean

all: fmt test build

build:
	go build -o bin/ ./cmd/...

test:
	go vet ./...
	go test ./...

# Runs the SurrealDB backend's conformance suites against a server, e.g.
#   surreal start --unauthenticated memory
#   make test-surrealdb SURREALDB=ws://127.0.0.1:8000
test-surrealdb:
	BEARING_TEST_SURREALDB=$(SURREALDB) go test -count=1 ./internal/surrealstore/

# Runs the same suites against embedded SurrealDB. Build libsurrealdb_c.a from
# github.com/surrealdb/surrealdb.c first (cargo build --release), then:
#   make test-embedded SURREALDB_LIB=/path/to/dir/with/libsurrealdb_c.a
test-embedded:
	CGO_ENABLED=1 CGO_LDFLAGS="-L$(SURREALDB_LIB)" go test -count=1 -tags surrealembed ./internal/surrealstore/ ./pkg/store/

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)
	go vet ./...

clean:
	rm -rf bin
