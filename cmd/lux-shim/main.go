// Command lux-shim runs as PID 1 inside every lux container. The runner
// mounts it read-only at /.lux/bin/lux-shim and makes it the entrypoint;
// images need nothing lux-specific. See internal/shim. `lux-shim diff` is
// the runner's live diff, run with podman exec (shim.Diff); `lux-shim sync`
// moves checkouts to new commits (shim.Sync).
package main

import (
	"os"

	"github.com/marcioapm/lux/internal/shim"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "diff" {
		os.Exit(shim.Diff(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "sync" {
		os.Exit(shim.Sync(os.Args[2:]))
	}
	os.Exit(shim.Main())
}
