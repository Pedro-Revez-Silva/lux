// Command lux-shim runs as PID 1 inside every lux container. The runner
// mounts it read-only at /.lux/bin/lux-shim and makes it the entrypoint;
// images need nothing lux-specific. See internal/shim.
//
// `lux-shim diff <json>` is the other use: the runner runs it, as the
// workload user, to compute a Run's repository diffs (internal/gitdiff).
package main

import (
	"os"

	"github.com/marcioapm/lux/internal/gitdiff"
	"github.com/marcioapm/lux/internal/shim"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "diff" {
		os.Exit(gitdiff.Main(os.Args[2:]))
	}
	os.Exit(shim.Main())
}
