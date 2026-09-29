package cli

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/client"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
)

// shellCommand is what lux shell runs: a login bash where there is one.
var shellCommand = []string{"/bin/sh", "-c", "exec bash -l 2>/dev/null || exec sh -l"}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <run>",
		Short: "Open a shell in a running Run",
		Long: `Open a login shell (bash, else sh) in a running Run's container, as its
workload user, with its environment, on a terminal. The same as
lux exec -t <run> -- /bin/sh -c 'exec bash -l 2>/dev/null || exec sh -l'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.exec(ctxOf(cmd), args[0], proto.StreamOpen{Command: shellCommand, TTY: true}, a.stdinTerminal())
		},
	}
}

func (a *app) serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"servers"},
		Short:   "A Run's servers: named ports, with commands lux runs",
		Long: `A server is a named port of a Run, optionally with a command lux runs in
its container (as its workload user, with its environment). Its preview URL
reaches it from a browser, wherever the Run is. A server's process stops
when its placement ends (a stop, a migration); the record stays.`,
	}
	cmd.AddCommand(a.serverAddCmd(), a.serverLsCmd(), a.serverLogsCmd(),
		a.serverActionCmd("start", "Start a server (the Run must be running)"),
		a.serverActionCmd("stop", "Stop a server"),
		a.serverActionCmd("restart", "Restart a server (as it is defined now)"),
		a.serverRmCmd(), a.serverWaitCmd())
	return cmd
}

// noServer is the API's not_found for a server (exit code 3).
func noServer(name string) error {
	return &client.APIError{Status: 404, Code: "not_found", Message: fmt.Sprintf("the Run has no server %q", name)}
}

func serverPath(run string, rest ...string) string {
	p := "/v1/runs/" + run + "/servers"
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p
}

func (a *app) serverAddCmd() *cobra.Command {
	var workdir string
	var envs []string
	var noStart bool
	cmd := &cobra.Command{
		Use:   "add <run> <name> <port> [-- command...]",
		Short: "Add a server to a Run, and start it",
		Long: `Add a server to a Run: a name, the port it listens on in the container, and
optionally the command that serves it. With a command it starts now (the Run
must be running) unless --no-start.`,
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("port: %q is not a number", args[2])
			}
			in := server.ServerInput{Name: args[1], Port: port, Workdir: workdir}
			if len(args) > 3 {
				in.Command = args[3:]
			}
			if len(envs) > 0 {
				in.Env = map[string]string{}
				for _, e := range envs {
					k, v, ok := strings.Cut(e, "=")
					if !ok || k == "" {
						return fmt.Errorf("--env %q: want K=V", e)
					}
					in.Env[k] = v
				}
			}
			if noStart {
				f := false
				in.Start = &f
			}
			var sv server.RunServer
			if err := a.c.Do(ctxOf(cmd), "POST", serverPath(args[0]), in, &sv); err != nil {
				return err
			}
			return a.printServer(sv)
		},
	}
	cmd.Flags().StringVar(&workdir, "workdir", "", "where the command runs (relative: to the workload's workdir)")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "K=V for its command (repeatable)")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "add it without starting it")
	return cmd
}

func (a *app) printServer(sv server.RunServer) error {
	if a.output == "json" {
		return a.json(sv)
	}
	fmt.Fprintf(a.stdout, "%s %s\n", sv.Name, serverState(sv))
	return nil
}

// serverState is a server's state in words: exited with its code, stopped
// with why.
func serverState(sv server.RunServer) string {
	switch {
	case sv.State == "exited" && sv.ExitCode != nil:
		return fmt.Sprintf("exited(%d)", *sv.ExitCode)
	case sv.State == "stopped" && sv.StopReason != nil:
		return "stopped (" + *sv.StopReason + ")"
	}
	return sv.State
}

func (a *app) listServers(ctx context.Context, run string) ([]server.RunServer, error) {
	var out struct {
		Servers []server.RunServer `json:"servers"`
	}
	err := a.c.Do(ctx, "GET", serverPath(run), nil, &out)
	return out.Servers, err
}

// getServer is one of a Run's servers, by name.
func (a *app) getServer(ctx context.Context, run, name string) (server.RunServer, error) {
	servers, err := a.listServers(ctx, run)
	if err != nil {
		return server.RunServer{}, err
	}
	i := slices.IndexFunc(servers, func(sv server.RunServer) bool { return sv.Name == name })
	if i < 0 {
		return server.RunServer{}, noServer(name)
	}
	return servers[i], nil
}

// serverURL is a server's preview URL, or "" without one.
func serverURL(sv server.RunServer) string {
	if sv.URL == nil {
		return ""
	}
	return *sv.URL
}

func (a *app) serverLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls <run> [name]",
		Aliases: []string{"list"},
		Short:   "List a Run's servers (or show one)",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				sv, err := a.getServer(ctxOf(cmd), args[0], args[1])
				if err != nil {
					return err
				}
				if a.output == "json" {
					return a.json(sv)
				}
				a.serverTable([]server.RunServer{sv})
				return nil
			}
			servers, err := a.listServers(ctxOf(cmd), args[0])
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(servers)
			}
			a.serverTable(servers)
			return nil
		},
	}
}

func (a *app) serverTable(servers []server.RunServer) {
	rows := make([][]string, 0, len(servers))
	for _, sv := range servers {
		command := "-"
		if len(sv.Command) > 0 {
			command = strings.Join(sv.Command, " ")
			if len(command) > 40 {
				command = command[:39] + "…"
			}
		}
		rows = append(rows, []string{sv.Name, strconv.Itoa(sv.Port), serverState(sv), ago(&sv.Since), orDash(serverURL(sv)), command})
	}
	a.table("NAME\tPORT\tSTATE\tSINCE\tURL\tCOMMAND", rows)
}

func (a *app) serverActionCmd(action, short string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <run> <name>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var sv server.RunServer
			if err := a.c.Do(ctxOf(cmd), "POST", serverPath(args[0], args[1], action), nil, &sv); err != nil {
				return err
			}
			return a.printServer(sv)
		},
	}
}

func (a *app) serverRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <run> <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a server (stopping it first)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.c.Do(ctxOf(cmd), "DELETE", serverPath(args[0], args[1]), nil, nil); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(map[string]any{"removed": args[1]})
			}
			fmt.Fprintf(a.stdout, "%s removed\n", args[1])
			return nil
		},
	}
}

func (a *app) serverLogsCmd() *cobra.Command {
	var tail int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <run> <name>",
		Short: "Print a server's recent output",
		Long: `Print the last lines a server's command wrote (--tail), across placements.
With -f, its whole output instead, followed live (as lux logs <run> --server
<name> -f).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			if follow {
				_, err := a.followLogs(ctx, args[0], "", logOpts{stderr: true, server: args[1]})
				return err
			}
			var out server.ServerLog
			if err := a.c.Do(ctx, "GET", serverPath(args[0], args[1], "log")+"?tail="+strconv.Itoa(tail), nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(out)
			}
			for _, l := range out.Lines {
				w := a.stdout
				if l.Stream == "stderr" {
					w = a.stderr
				}
				fmt.Fprintln(w, l.Text)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 200, "how many of its last lines")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep following its output")
	return cmd
}

func (a *app) serverWaitCmd() *cobra.Command {
	var states string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <run> <name>",
		Short: "Wait for a server to reach a state (default: ready)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			want := strings.Split(states, ",")
			ctx, cancel := context.WithTimeout(ctxOf(cmd), timeout)
			defer cancel()
			for {
				sv, err := a.getServer(ctx, args[0], args[1])
				if err != nil {
					return err
				}
				if slices.Contains(want, sv.State) {
					return a.printServer(sv)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("server %s is %s, not %s, after %s", args[1], serverState(sv), states, timeout)
				case <-time.After(500 * time.Millisecond):
				}
			}
		},
	}
	cmd.Flags().StringVar(&states, "state", "ready", "states to wait for, comma-separated")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "give up after this long")
	return cmd
}
