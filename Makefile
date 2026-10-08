.PHONY: all build test test-surrealdb test-embedded fmt lint vet cover covergate vuln tools-test generate generate-check check clean

# Developer tools are pinned in tools/go.mod and built into bin/tools.
TOOLS := bin/tools
GOLANGCI_LINT := $(TOOLS)/golangci-lint
GOVULNCHECK := $(TOOLS)/govulncheck
COVERGATE := $(TOOLS)/covergate
BUF := $(TOOLS)/buf
PROTOC_GEN_GO := $(TOOLS)/protoc-gen-go
PROTOC_GEN_JSONSCHEMA := $(TOOLS)/protoc-gen-jsonschema
PROTO_TOOLS := $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_JSONSCHEMA)

# The coverage gate compares against the merge base with COVER_BASE. On a
# GitHub pull request that is the PR's base branch.
COVER_BASE ?= origin/$(or $(GITHUB_BASE_REF),main)
COVER_MIN ?= 80
COVER_TEST := go test -count=1 -timeout 20m -coverpkg=./... -coverprofile
# In CI a missing base or an unmeasurable baseline fails the gate instead of
# skipping it.
COVER_FLAGS ?= $(if $(GITHUB_ACTIONS),-require-base -require-baseline)

all: fmt test build

# check is exactly what CI runs. vuln goes last because it needs the
# network (vuln.go.dev).
check: generate-check lint vet tools-test cover covergate build vuln

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

# The tools module is separate, so ./... does not reach it; lint it with the
# same config from inside it.
fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...
	cd tools && ../$(GOLANGCI_LINT) fmt --config ../.golangci.yml ./...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...
	cd tools && ../$(GOLANGCI_LINT) run --config ../.golangci.yml ./...

vet:
	go vet ./...

# The tools module is separate, so ./... above does not reach it.
tools-test:
	go -C tools vet ./...
	go -C tools test ./...

# generate formats and lints proto/ and regenerates gen/go and
# gen/jsonschema (see buf.gen.yaml). Commit the result.
generate: $(PROTO_TOOLS)
	$(BUF) format -w
	$(BUF) lint
	$(BUF) generate

# generate-check fails if proto/ isn't formatted or doesn't lint, or if gen/
# differs from what buf generate writes.
generate-check: $(PROTO_TOOLS)
	$(BUF) format --diff --exit-code
	$(BUF) lint
	$(BUF) generate
	@git diff --exit-code -- gen || { echo 'gen/ is stale: run make generate and commit the result'; exit 1; }
	@test -z "$$(git status --porcelain --untracked-files=all -- gen)" || \
		{ git status --porcelain --untracked-files=all -- gen; echo 'gen/ has uncommitted files: run make generate and commit the result'; exit 1; }

cover:
	$(COVER_TEST)=cover.out ./...

covergate: cover $(COVERGATE)
	$(COVERGATE) -profile cover.out -base $(COVER_BASE) -min $(COVER_MIN) $(COVER_FLAGS) -test '$(COVER_TEST)={profile} ./...'

vuln: $(GOVULNCHECK)
	$(GOVULNCHECK) ./...

$(GOLANGCI_LINT): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ github.com/golangci/golangci-lint/v2/cmd/golangci-lint

$(GOVULNCHECK): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ golang.org/x/vuln/cmd/govulncheck

$(BUF): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ github.com/bufbuild/buf/cmd/buf

$(PROTOC_GEN_GO): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ google.golang.org/protobuf/cmd/protoc-gen-go

$(PROTOC_GEN_JSONSCHEMA): tools/go.mod tools/go.sum
	go -C tools build -o ../$(TOOLS)/ github.com/bufbuild/protoschema-plugins/cmd/protoc-gen-jsonschema

$(COVERGATE): tools/go.mod $(wildcard tools/covergate/*.go)
	go -C tools build -o ../$(TOOLS)/ ./covergate

clean:
	rm -rf bin cover.out
