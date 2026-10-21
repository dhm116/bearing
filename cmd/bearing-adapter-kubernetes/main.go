// Command bearing-adapter-kubernetes serves the Kubernetes adapter over the
// Bearing adapter service where BEARING_ADAPTER_LISTEN says.
package main

import (
	"os"

	"bearing.example/adapters/kubernetes"
	"bearing.example/pkg/adapter"
)

func main() {
	os.Exit(adapter.Main("bearing-adapter-kubernetes", kubernetes.Version, kubernetes.New()))
}
