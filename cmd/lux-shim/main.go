// Command lux-shim runs as PID 1 inside every lux container. The runner
// mounts it read-only at /.lux/bin/lux-shim and makes it the entrypoint;
// images need nothing lux-specific. See internal/shim. `lux-shim diff` is
// the runner's live diff, run with podman exec (shim.Diff); `lux-shim sync`
// moves checkouts to new commits (shim.Sync); `lux-shim publish FILE` is
// the workload's: it makes FILE an artifact of the Run (shim.Publish);
// `lux-shim oci-defaults` is the OCI hook giving nested containers the
// Run's limits by default (shim.OCIDefaults); `lux-shim capabilities` lists the features the runner may rely on.
package main

import (
	"os"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/shim"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "diff" {
		os.Exit(shim.Diff(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "sync" {
		os.Exit(shim.Sync(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "publish" {
		os.Exit(shim.Publish(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == proto.ShimOCIDefaultsCmd {
		os.Exit(shim.OCIDefaults(os.Args[2:], os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == proto.ShimCapabilitiesCmd {
		os.Exit(shim.Capabilities(os.Stdout))
	}
	os.Exit(shim.Main())
}
