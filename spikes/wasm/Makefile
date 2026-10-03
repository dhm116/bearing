# WASM adapter spike. See docs/spikes/wasm-adapters.md.
#
# TINYGO and WASM_OPT are optional; targets that need them are skipped when
# the tools are missing. OUT is where modules and binaries land.

OUT      ?= out
TINYGO   ?= tinygo
WASM_OPT ?= wasm-opt
ROOT     := $(abspath ../..)

GO_WASM     = GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -ldflags="-s -w"
TINYGO_WASM = $(TINYGO) build -target wasip1 -buildmode=c-shared -no-debug

.PHONY: all modules modules-go modules-tinygo opt lean bench clean

all: modules opt bench

$(OUT):
	mkdir -p $(OUT)

# lean/ holds generated copies of adapters/github, pkg/adapter/protocol.go and
# guest/exports with pkg/telemetry swapped for an API-only shim (no OTel SDK,
# no exporters). pkg/adapter is copied too because it imports pkg/telemetry.
# It measures what the module would cost if pkg/telemetry were split into an
# API part for adapters and a setup part for binaries.
SWAP = sed -e 's\#"bearing.example/pkg/telemetry"\#telemetry "bearing.example/spikes/wasm/guest/lean/telemetry"\#' \
	-e 's\#"bearing.example/pkg/adapter"\#"bearing.example/spikes/wasm/guest/lean/adapter"\#'
lean:
	rm -rf guest/lean/githubcopy guest/lean/adapter guest/lean/exportscopy
	mkdir -p guest/lean/githubcopy guest/lean/adapter guest/lean/exportscopy
	$(SWAP) $(ROOT)/adapters/github/github.go > guest/lean/githubcopy/github.go
	$(SWAP) $(ROOT)/adapters/github/telemetry.go > guest/lean/githubcopy/telemetry.go
	$(SWAP) $(ROOT)/pkg/adapter/protocol.go > guest/lean/adapter/protocol.go
	$(SWAP) guest/exports/exports.go > guest/lean/exportscopy/exports.go
	gofmt -w guest/lean

modules: modules-go modules-tinygo

modules-go: $(OUT) lean
	cd guest && $(GO_WASM) -o ../$(OUT)/github.go.wasm ./github
	cd guest && $(GO_WASM) -o ../$(OUT)/github-lean.go.wasm ./githublean
	cd guest && $(GO_WASM) -tags protovalidate -o ../$(OUT)/github-lean-pv.go.wasm ./githublean
	cd guest && $(GO_WASM) -o ../$(OUT)/raw-github-lean.go.wasm ./githubraw
	cd aws && $(GO_WASM) -o ../$(OUT)/aws-sts.go.wasm .

modules-tinygo: $(OUT) lean
	-cd guest && $(TINYGO_WASM) -o ../$(OUT)/github.tinygo.wasm ./github
	-cd guest && $(TINYGO_WASM) -o ../$(OUT)/github-lean.tinygo.wasm ./githublean
	-cd guest && $(TINYGO_WASM) -tags protovalidate -o ../$(OUT)/github-lean-pv.tinygo.wasm ./githublean
	-cd guest && $(TINYGO_WASM) -o ../$(OUT)/raw-github-lean.tinygo.wasm ./githubraw
	-cd aws && $(TINYGO_WASM) -o ../$(OUT)/aws-sts.tinygo.wasm .

opt:
	for f in $(OUT)/*.wasm; do case $$f in *.opt.wasm) continue;; esac; \
		$(WASM_OPT) -Oz --enable-bulk-memory --enable-sign-ext --enable-nontrapping-float-to-int \
			--enable-mutable-globals -o $${f%.wasm}.opt.wasm $$f || true; done

bench:
	cd host && go run . -out ../$(OUT) -root $(ROOT)

clean:
	rm -rf $(OUT) guest/lean/githubcopy guest/lean/adapter guest/lean/exportscopy
