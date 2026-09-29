// Package cli is the lux command-line client.
//
// Every command that prints data takes -o json for scripts (and for the
// end-to-end tests, which drive lux through this CLI).
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/client"
	"github.com/marcioapm/lux/internal/version"
)

type app struct {
	url    string
	key    string
	tenant string
	output string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	c      *client.Client
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	return (&app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}).main(args)
}

func (a *app) main(args []string) int {
	root := a.root()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ec exitCode
	if errors.As(err, &ec) {
		return int(ec)
	}
	fmt.Fprintln(a.stderr, "lux:", err)
	var ae *client.APIError
	if errors.As(err, &ae) {
		if ae.Code == "tenant_required" {
			// luxd's message speaks of ?tenant=; say how the CLI sets it.
			fmt.Fprintln(a.stderr, "lux: name one with --tenant, LUX_TENANT, or tenant in ~/.config/lux/config.toml")
		}
		switch {
		case ae.Status == 404:
			return 3
		case ae.Status == 409 || ae.Status == 422:
			return 4
		case ae.Code == "quota_exceeded":
			return 5
		}
	}
	return 1
}

// exitCode is returned by commands that propagate a Run's exit code.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

type config struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
	Tenant string `toml:"tenant"`
}

func loadConfig() config {
	var c config
	dir, err := os.UserConfigDir()
	if err != nil {
		return c
	}
	b, err := os.ReadFile(filepath.Join(dir, "lux", "config.toml"))
	if err != nil {
		return c
	}
	_ = toml.Unmarshal(b, &c)
	return c
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:           "lux",
		Short:         "Run, steer, stop and resume isolated workloads",
		Long:          "lux runs workloads — commands or coding agents — in isolated containers on any host,\nstreams their output, lets you steer them, and stops and resumes them anywhere.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg := loadConfig()
			if a.url == "" {
				a.url = cmp.Or(os.Getenv("LUX_URL"), cfg.URL)
			}
			if a.key == "" {
				a.key = cmp.Or(os.Getenv("LUX_API_KEY"), cfg.APIKey)
			}
			if a.url == "" {
				return errors.New("no lux URL: set LUX_URL, --url, or url in ~/.config/lux/config.toml")
			}
			if a.key == "" {
				return errors.New("no API key: set LUX_API_KEY, --api-key, or api_key in ~/.config/lux/config.toml")
			}
			if a.output != "text" && a.output != "json" {
				return fmt.Errorf("-o must be text or json")
			}
			a.c = client.New(a.url, a.key)
			a.c.Tenant = cfg.Tenant
			if env, ok := os.LookupEnv("LUX_TENANT"); ok {
				a.c.Tenant = env
			}
			// An explicitly empty flag overrides both the environment and config.
			if cmd.Flags().Changed("tenant") {
				a.c.Tenant = a.tenant
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&a.url, "url", "", "luxd URL (env LUX_URL)")
	root.PersistentFlags().StringVar(&a.key, "api-key", "", "API key (env LUX_API_KEY)")
	root.PersistentFlags().StringVar(&a.tenant, "tenant", "", "with an operator key: act on this tenant only, by id or name (env LUX_TENANT, config tenant); --tenant \"\" means all tenants")
	root.PersistentFlags().StringVarP(&a.output, "output", "o", "text", "output format: text | json")
	root.AddCommand(
		a.runCmd(), a.lsCmd(), a.getCmd(), a.logsCmd(), a.eventsCmd(), a.steerCmd(), a.interruptCmd(),
		a.stopCmd(), a.resumeCmd(), a.cancelCmd(), a.waitCmd(), a.pushCmd(), a.snapshotsCmd(),
		a.artifactsCmd(), a.execCmd(), a.attachCmd(), a.portForwardCmd(), a.hostsCmd(), a.poolsCmd(),
		a.tenantsCmd(), a.statusCmd(), a.historyCmd(), a.costCmd(), a.costsCmd(), a.migrateCmd(),
	)
	return root
}

func (a *app) json(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func ctxOf(cmd *cobra.Command) context.Context { return cmd.Context() }

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	d := time.Since(*t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
