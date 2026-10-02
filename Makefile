.PHONY: all build test test-surrealdb test-embedded fmt lint vet cover covergate vuln tools-test check clean

# Developer tools are pinned in tools/go.mod and built into bin/tools.
TOOLS := bin/tools
GOLANGCI_LINT := $(TOOLS)/golangci-lint
GOVULNCHECK := $(TOOLS)/govulncheck
COVERGATE := $(TOOLS)/covergate

# The coverage gate compares against the merge base with COVER_BASE. On a
# GitHub pull request that is the PR's base branch.
COVER_BASE ?= origin/$(or $(GITHUB_BASE_REF),main)
COVER_MIN ?= 80
COVER_TEST := go test -count=1 -coverpkg=./... -coverprofile

all: fmt test build

# check is exactly what CI runs.
check: lint vet tools-test cover covergate vuln build

build:
	go build -o bin/ ./cmd/...

test:
	go vet ./...
	go test ./...

# Runs the SurrealDB backend's conformance suites against a server, e.g.
#   surreal start --user root --pass root memory
#   make test-surrealdb SURREALDB=ws://127.0.0.1:8000 SURREALDB_USER=root SURREALDB_PASS=root
test-surrealdb:
	BEARING_TEST_SURREALDB=$(SURREALDB) BEARING_TEST_SURREALDB_USER=$(SURREALDB_USER) \
		BEARING_TEST_SURREALDB_PASS=$(SURREALDB_PASS) \
		go test -count=1 ./internal/surrealstore/ ./pkg/store/

# Runs the same suites against embedded SurrealDB. Build libsurrealdb_c.a from
# github.com/surrealdb/surrealdb.c first (cargo build --release), then:
#   make test-embedded SURREALDB_LIB=/path/to/dir/with/libsurrealdb_c.a
test-embedded:
	CGO_ENABLED=1 CGO_LDFLAGS="-L$(SURREALDB_LIB)" go test -count=1 -tags surrealembed ./internal/surrealstore/ ./pkg/store/

fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...
	gofmt -w tools

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...
	@test -z "$$(gofmt -l tools)" || (gofmt -l tools && echo "run make fmt" && exit 1)

vet:
	go vet ./...

# The tools module is separate, so ./... above does not reach it.
tools-test:
	go -C tools vet ./...
	go -C tools test ./...

cover:
	$(COVER_TEST)=cover.out ./...

covergate: $(COVERGATE)
	$(COVERGATE) -profile cover.out -base $(COVER_BASE) -min $(COVER_MIN) -test '$(COVER_TEST)={profile} ./...'

vuln: $(GOVULNCHECK)
	$(GOVULNCHECK) ./...

$(GOLANGCI_LINT): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ github.com/golangci/golangci-lint/v2/cmd/golangci-lint

$(GOVULNCHECK): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ golang.org/x/vuln/cmd/govulncheck

$(COVERGATE): tools/go.mod $(wildcard tools/covergate/*.go)
	go -C tools build -o ../$(TOOLS)/ ./covergate

clean:
	rm -rf bin cover.out
