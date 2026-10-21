// Command gitopsify turns what runs in a Kubernetes cluster into a GitOps
// repository.
package main

import (
	"os"

	"github.com/hayone1/gitopsify/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
