package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"net/http"
)

func (a *app) checkpointCmd() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{Use: "checkpoint <run>", Short: "Save committed local branches and HEAD for other Runs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var out struct {
			Checkpoints []struct {
				ID, Repo, Head string
				Size           int64
			} `json:"checkpoints"`
		}
		path := "/v1/runs/" + args[0] + "/checkpoint"
		if wait {
			path += "?wait=true"
		}
		if err := a.c.Do(ctxOf(cmd), http.MethodPost, path, nil, &out); err != nil {
			return err
		}
		if a.output == "json" {
			return a.json(out)
		}
		for _, c := range out.Checkpoints {
			fmt.Fprintf(a.stdout, "%s %s %s %d bytes\n", c.Repo, c.ID, c.Head, c.Size)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the checkpoint to upload")
	return cmd
}
