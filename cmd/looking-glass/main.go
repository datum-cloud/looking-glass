// Command looking-glass runs the Datum Cloud Looking Glass services.
package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"go.datum.net/looking-glass/internal/cmd"
	// In-tree router drivers register their platforms.
	_ "go.datum.net/looking-glass/pkg/driver/frr"
)

func main() {
	// The first SIGTERM or SIGINT cancels the context and starts a graceful
	// shutdown; a second one exits immediately.
	ctx := ctrl.SetupSignalHandler()
	os.Exit(cmd.Execute(ctx, os.Args[1:], os.Stderr))
}
