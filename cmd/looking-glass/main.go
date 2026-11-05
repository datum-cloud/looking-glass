// Command looking-glass runs the Datum Cloud Looking Glass services.
package main

import (
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"go.datum.net/looking-glass/internal/cmd"
)

func main() {
	// The first SIGTERM or SIGINT cancels the context and starts a graceful
	// shutdown; a second one exits immediately.
	ctx := ctrl.SetupSignalHandler()
	if err := cmd.New(ctx).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
