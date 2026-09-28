// Command bearing-adapter-github serves the GitHub adapter over the Bearing
// adapter protocol on stdin and stdout.
package main

import (
	"fmt"
	"os"

	"bearing.example/adapters/github"
	"bearing.example/pkg/adapter"
)

func main() {
	if err := adapter.ServeStdio(github.New()); err != nil {
		fmt.Fprintln(os.Stderr, "bearing-adapter-github:", err)
		os.Exit(1)
	}
}
