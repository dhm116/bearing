// Command bearing-adapter-github serves the GitHub adapter over the adapter
// service where BEARING_ADAPTER_LISTEN says (pkg/adapter.Main). Telemetry goes
// to stderr or OTLP.
package main

import (
	"os"

	"bearing.example/adapters/github"
	"bearing.example/pkg/adapter"
)

func main() {
	os.Exit(adapter.Main("bearing-adapter-github", github.Version, github.New()))
}
