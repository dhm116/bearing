// Command bearing-adapter-authentik serves the Authentik directory adapter
// over the Bearing adapter service where BEARING_ADAPTER_LISTEN says.
package main

import (
	"os"

	"bearing.example/adapters/authentik"
	"bearing.example/pkg/adapter"
)

func main() {
	os.Exit(adapter.Main("bearing-adapter-authentik", authentik.Version, authentik.New()))
}
