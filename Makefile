.PHONY: all build test test-postgres bench fmt lint vet cover covergate covergate-base covergate-report vuln tools-test generate generate-check check static clean

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

COVER_OUT ?= cover.out
# A hung CI job dumps its goroutines before the job's 20-minute limit.
COVER_TEST := go test -count=1 -timeout $(if $(GITHUB_ACTIONS),15m,20m) -coverpkg=./... -coverprofile
# In CI a missing base or an unmeasurable baseline fails the gate instead of
# skipping it.
COVER_FLAGS ?= $(if $(GITHUB_ACTIONS),-require-base -require-baseline)

all: fmt test build

# check is everything CI runs, in one process. CI runs the same steps as
# parallel jobs so a push is answered in the time of the slowest one: static
# (no test run), cover (the tests, with PostgreSQL), covergate-base (the same
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

# Runs the PostgreSQL backend's suites against a server, e.g.
#   docker run --rm -p 127.0.0.1:5432:5432 -e POSTGRES_PASSWORD=postgres pgvector/pgvector:pg16
#   make test-postgres POSTGRES=postgres://postgres@127.0.0.1:5432/postgres POSTGRES_PASS=postgres
# The role must be allowed to create roles and schemas. Set POSTGRES_SCOPED=1
# to run the stores as a role without administrator rights, as CI does.
test-postgres:
	BEARING_TEST_POSTGRES=$(POSTGRES) BEARING_TEST_POSTGRES_PASSWORD=$(POSTGRES_PASS) \
		BEARING_TEST_POSTGRES_SCOPED=$(POSTGRES_SCOPED) \
		go test -count=1 -timeout 30m ./internal/pgstore/ ./pkg/store/

# Runs the store benchmark (docs/benchmarks/README.md), e.g.
#   make bench BENCH='load --store postgres://bench@127.0.0.1:5432/bench?schema=main --facts 1000000' BEARING_STORE_PASSWORD=...
# It is not part of check: the full run takes hours.
bench:
	go run ./cmd/bearing-bench $(BENCH)

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
	$(COVER_TEST)=$(COVER_OUT) ./...

# BASE_PROFILE is where covergate-base writes the merge base's coverage and
# covergate-report reads it (COVER_OUT and BASE_PROFILE may be comma-separated
# lists of profiles there, merged). Left empty, covergate-report measures the
# merge base itself, which costs as much as `make cover`.
BASE_PROFILE ?=

covergate: cover covergate-report

covergate-report: $(COVERGATE)
	$(COVERGATE) -profile $(COVER_OUT) -base $(COVER_BASE) -min $(COVER_MIN) $(COVER_FLAGS) \
		$(if $(BASE_PROFILE),-base-profile $(BASE_PROFILE)) -test '$(COVER_TEST)={profile} ./...'

# covergate-base runs the tests at the merge base and writes their profile to
# BASE_PROFILE, unless covergate-report would skip the comparison.
covergate-base: $(COVERGATE)
	test -n '$(BASE_PROFILE)'
	$(COVERGATE) -base $(COVER_BASE) $(COVER_FLAGS) -measure-base $(BASE_PROFILE) -test '$(COVER_TEST)={profile} ./...'

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
