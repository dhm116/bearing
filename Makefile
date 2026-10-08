.PHONY: all build test test-surrealdb test-embedded fmt lint vet cover covergate covergate-base covergate-report vuln tools-test generate generate-check check static clean

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

# SHARD runs part of the tests, so CI can run the parts on separate runners
# and covergate merges their profiles. The SurrealDB conformance case below
# applies a 50,000-item ChangeSet per list, which takes a server most of a
# core for a minute each; those cases are split in two shards and run
# alone, and the rest shard runs everything else. A rename of the case or
# its subtests only moves tests to the rest shard (slower, never skipped).
# TestGraphConformance is the name only internal/surrealstore gives its run
# of the suite; keep it that way or the filters below match nothing.
# Empty SHARD runs everything, as `make check` does.
COVER_HEAVY := ^TestGraphConformance/^Apply_refuses_more_items_than_the_count_limits
COVER_LIMITS_A := fact_timelines|conflict_timelines|state_entries|mints
COVER_LIMITS_B := support_timelines|issue_timelines|binding_timelines
ifeq ($(SHARD),)
COVER_FILTER :=
COVER_PKGS := ./...
else ifeq ($(SHARD),rest)
COVER_FILTER := -skip "$(COVER_HEAVY)/^($(COVER_LIMITS_A)|$(COVER_LIMITS_B))"
COVER_PKGS := ./...
else ifeq ($(SHARD),limits-a)
COVER_FILTER := -run "$(COVER_HEAVY)/^($(COVER_LIMITS_A))"
COVER_PKGS := ./internal/surrealstore/
else ifeq ($(SHARD),limits-b)
COVER_FILTER := -run "$(COVER_HEAVY)/^($(COVER_LIMITS_B))"
COVER_PKGS := ./internal/surrealstore/
else
$(error unknown SHARD "$(SHARD)": want rest, limits-a or limits-b)
endif
COVER_OUT ?= cover.out
# A shard that hangs dumps its goroutines before the job's 20-minute limit.
COVER_TIMEOUT := $(if $(SHARD),15m,20m)
COVER_TEST := go test -count=1 -timeout $(COVER_TIMEOUT) -coverpkg=./... $(COVER_FILTER) -coverprofile
# In CI a missing base or an unmeasurable baseline fails the gate instead of
# skipping it.
COVER_FLAGS ?= $(if $(GITHUB_ACTIONS),-require-base -require-baseline)

all: fmt test build

# check is everything CI runs, in one process. CI runs the same steps as
# parallel jobs so a push is answered in the time of the slowest one: static
# (no test run), cover (the tests, with SurrealDB), covergate-base (the same
# tests at the merge base) and covergate-report (compares the two profiles).
# vuln goes last because it needs the network (vuln.go.dev).
check: generate-check lint vet tools-test cover covergate build vuln

# static is check without the test run and the coverage gate.
static: generate-check lint vet tools-test build vuln

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
		go test -count=1 -timeout 30m ./internal/surrealstore/ ./pkg/store/

# Runs the same suites against embedded SurrealDB. Build libsurrealdb_c.a from
# github.com/surrealdb/surrealdb.c first (cargo build --release), then:
#   make test-embedded SURREALDB_LIB=/path/to/dir/with/libsurrealdb_c.a
test-embedded:
	CGO_ENABLED=1 CGO_LDFLAGS="-L$(SURREALDB_LIB)" go test -count=1 -timeout 30m -tags surrealembed ./internal/surrealstore/ ./pkg/store/

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
	$(COVER_TEST)=$(COVER_OUT) $(COVER_PKGS)

# BASE_PROFILE is where covergate-base writes the merge base's coverage and
# covergate-report reads it (COVER_OUT and BASE_PROFILE are comma-separated
# lists there, one file per shard). Left empty, covergate-report measures the
# merge base itself, which costs as much as `make cover`.
BASE_PROFILE ?=

covergate: cover covergate-report

covergate-report: $(COVERGATE)
	$(COVERGATE) -profile $(COVER_OUT) -base $(COVER_BASE) -min $(COVER_MIN) $(COVER_FLAGS) \
		$(if $(BASE_PROFILE),-base-profile $(BASE_PROFILE)) -test '$(COVER_TEST)={profile} $(COVER_PKGS)'

# covergate-base runs the tests at the merge base and writes their profile to
# BASE_PROFILE, unless covergate-report would skip the comparison.
covergate-base: $(COVERGATE)
	test -n '$(BASE_PROFILE)'
	$(COVERGATE) -base $(COVER_BASE) $(COVER_FLAGS) -measure-base $(BASE_PROFILE) -test '$(COVER_TEST)={profile} $(COVER_PKGS)'

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
