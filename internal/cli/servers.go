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
	"github.com/marcioapm/lux/internal/spec"
)

// shellCommand is what lux shell runs: a login bash where there is one.
var shellCommand = []string{"/bin/sh", "-c", "command -v bash >/dev/null && exec bash -l; exec /bin/sh -l"}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <run>",
		Short: "Open a shell in a running Run",
		Long: `Open a login shell (bash, else sh) in a running Run's container, as its
workload user, with its environment, on a terminal. The same as
lux exec -t <run> -- /bin/sh -c 'command -v bash >/dev/null && exec bash -l; exec /bin/sh -l'.`,
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
		Short:   "Servers: named URLs that reach a port in a Run",
		Long: `A server is a named URL that reaches a port in a Run, optionally with a
command lux runs in its container (as its workload user, with its
environment). It is the tenant's own (srv_…): attached to at most one Run at
a time, it outlives Runs (lifetime owner) or ends with its Run (lifetime
run, the default of lux server add). Its preview URL is stable for its life.
Its process stops when its placement ends (a stop, a migration) and starts
again on the Run's next placement, unless someone stopped it.

A server is named by its id (srv_…), or by its Run and its name.

  lux server create web 3000 --wake request --hostname web.pr9.<domain> -- npm run dev
  lux server ls [--state asleep] [-l k=v]      # the tenant's servers
  lux server ls <run>                          # a Run's servers
  lux server attach srv_… <run>; lux server detach srv_…
  lux server start|stop|restart srv_…  (or <run> <name>)`,
	}
	cmd.AddCommand(a.serverAddCmd(), a.serverCreateCmd(), a.serverLsCmd(), a.serverShowCmd(), a.serverUpdateCmd(), a.serverLogsCmd(),
		a.serverActionCmd("start", "Start a server (now if its Run runs, else at its next placement)"),
		a.serverActionCmd("stop", "Stop a server (it stays stopped on later placements until started)"),
		a.serverActionCmd("restart", "Restart a server (as it is defined now)"),
		a.serverRmCmd(), a.serverWaitCmd(), a.serverAttachCmd(), a.serverDetachCmd())
	return cmd
}

// isServerID: a server named by its id rather than by Run and name.
func isServerID(s string) bool { return strings.HasPrefix(s, "srv_") }

func tenantServerPath(id string, rest ...string) string {
	p := "/v1/servers/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

// doTenantServer sends a request whose answer is a server, and prints it.
func (a *app) doTenantServer(ctx context.Context, method, path string, body any) error {
	var sv server.TenantServer
	if err := a.c.Do(ctx, method, path, body, &sv); err != nil {
		return err
	}
	return a.printTenantServer(sv)
}

func (a *app) printTenantServer(sv server.TenantServer) error {
	if a.output == "json" {
		return a.json(sv)
	}
	fmt.Fprintf(a.stdout, "%s %s %s %s\n", sv.ID, sv.Name, sv.State, orDash(strPtr(sv.URL)))
	return nil
}

func strPtr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parseKVs reads K=V arguments (labels, env).
func parseKVs(flag string, kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, e := range kvs {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--%s %q: want K=V", flag, e)
		}
		out[k] = v
	}
	return out, nil
}

// serverFlags are create's and update's.
type serverFlags struct {
	workdir, hostname, wake, lifetime, runID string
	idleAfter, wakeTimeout, expireAfter      string
	envs, labels, afterSync                  []string
}

func (f *serverFlags) add(cmd *cobra.Command, create bool) {
	cmd.Flags().StringVar(&f.workdir, "workdir", "", "where the command runs (relative: to the workload's workdir)")
	cmd.Flags().StringArrayVar(&f.envs, "env", nil, "K=V for its command (repeatable)")
	cmd.Flags().StringArrayVarP(&f.labels, "label", "l", nil, "K=V label (repeatable)")
	cmd.Flags().StringVar(&f.wake, "wake", "", "request: a request while nothing serves it asks its owner for a Run (server.wake_requested); never")
	cmd.Flags().StringVar(&f.idleAfter, "idle-after", "", "server.idle after this long without a request (default 10m; 0: never)")
	cmd.Flags().StringVar(&f.wakeTimeout, "wake-timeout", "", "how long a wake waits before its page says no answer (default 5m)")
	cmd.Flags().StringVar(&f.expireAfter, "expire-after", "", "lifetime owner: delete it after this long without a request (default 30d; 0: never)")
	cmd.Flags().StringVar(&f.lifetime, "lifetime", "", "owner: kept until deleted; run: deleted when its Run finishes for good")
	cmd.Flags().StringArrayVar(&f.afterSync, "after-sync", nil, "argv word run before the command after a repository sync (repeat for each word)")
	if create {
		cmd.Flags().StringVar(&f.hostname, "hostname", "", "its preview host name, under the preview domain (default <name>-<id part>.<domain>)")
		cmd.Flags().StringVar(&f.runID, "run", "", "attach it to this Run at once")
	}
}

func dur(s string) *spec.Duration {
	if s == "" {
		return nil
	}
	var d spec.Duration
	if err := d.UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
		return nil
	}
	return &d
}

func (a *app) serverCreateCmd() *cobra.Command {
	var f serverFlags
	cmd := &cobra.Command{
		Use:   "create <name> <port> [-- command...]",
		Short: "Create a server of the tenant (attached to no Run, or --run)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("port: %q is not a number", args[1])
			}
			env, err := parseKVs("env", f.envs)
			if err != nil {
				return err
			}
			labels, err := parseKVs("label", f.labels)
			if err != nil {
				return err
			}
			in := server.CreateServerInput{Name: args[0], Port: port, Workdir: f.workdir, Env: env, Labels: labels, Hostname: f.hostname,
				Wake: f.wake, Lifetime: f.lifetime, RunID: f.runID, AfterSync: f.afterSync,
				IdleAfter: dur(f.idleAfter), WakeTimeout: dur(f.wakeTimeout), ExpireAfter: dur(f.expireAfter)}
			if len(args) > 2 {
				in.Command = args[2:]
			}
			return a.doTenantServer(ctxOf(cmd), "POST", "/v1/servers", in)
		},
	}
	f.add(cmd, true)
	return cmd
}

func (a *app) serverUpdateCmd() *cobra.Command {
	var f serverFlags
	var port int
	var command []string
	cmd := &cobra.Command{
		Use:   "update <srv_id> [-- command...]",
		Short: "Change a server (what is given; command, port, workdir, env apply at its next start)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := server.PatchServerInput{IdleAfter: dur(f.idleAfter), WakeTimeout: dur(f.wakeTimeout), ExpireAfter: dur(f.expireAfter)}
			if cmd.Flags().Changed("port") {
				in.Port = &port
			}
			if len(args) > 1 {
				command = args[1:]
				in.Command = &command
			}
			if cmd.Flags().Changed("workdir") {
				in.Workdir = &f.workdir
			}
			if cmd.Flags().Changed("wake") {
				in.Wake = &f.wake
			}
			if cmd.Flags().Changed("lifetime") {
				in.Lifetime = &f.lifetime
			}
			if cmd.Flags().Changed("after-sync") {
				in.AfterSync = &f.afterSync
			}
			if env, err := parseKVs("env", f.envs); err != nil {
				return err
			} else if env != nil {
				in.Env = &env
			}
			if labels, err := parseKVs("label", f.labels); err != nil {
				return err
			} else if labels != nil {
				in.Labels = &labels
			}
			return a.doTenantServer(ctxOf(cmd), "PATCH", tenantServerPath(args[0]), in)
		},
	}
	f.add(cmd, false)
	cmd.Flags().IntVar(&port, "port", 0, "the port it listens on")
	return cmd
}

func (a *app) serverShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <srv_id | hostname>",
		Short: "Show a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sv, err := a.tenantServer(ctxOf(cmd), args[0])
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(sv)
			}
			kv := [][2]string{
				{"id", sv.ID}, {"name", sv.Name}, {"state", sv.State}, {"url", orDash(strPtr(sv.URL))},
				{"run", orDash(strPtr(sv.RunID))}, {"process", sv.Process}, {"desired", sv.Desired},
				{"port", strconv.Itoa(sv.Port)}, {"command", orDash(strings.Join(sv.Command, " "))},
				{"after sync", orDash(strings.Join(sv.AfterSync, " "))},
				{"wake", sv.Wake}, {"idle after", sv.IdleAfter.String()}, {"wake timeout", sv.WakeTimeout.String()},
				{"lifetime", sv.Lifetime}, {"owner", sv.Owner}, {"last request", ago(sv.LastRequestAt)}, {"wakes", strconv.Itoa(sv.Wakes)},
			}
			if sv.IdleAt != nil {
				kv = append(kv, [2]string{"idle in", time.Until(*sv.IdleAt).Round(time.Second).String()})
			}
			if sv.ExpiresAt != nil {
				kv = append(kv, [2]string{"expires", sv.ExpiresAt.Local().Format(time.RFC3339)})
			}
			if len(sv.Labels) > 0 {
				var ls []string
				for k, v := range sv.Labels {
					ls = append(ls, k+"="+v)
				}
				slices.Sort(ls)
				kv = append(kv, [2]string{"labels", strings.Join(ls, " ")})
			}
			for _, p := range kv {
				fmt.Fprintf(a.stdout, "%-13s %s\n", p[0]+":", p[1])
			}
			return nil
		},
	}
}

// tenantServer finds a server by id or hostname.
func (a *app) tenantServer(ctx context.Context, ref string) (server.TenantServer, error) {
	var sv server.TenantServer
	if isServerID(ref) {
		return sv, a.c.Do(ctx, "GET", tenantServerPath(ref), nil, &sv)
	}
	var out struct {
		Servers []server.TenantServer `json:"servers"`
	}
	if err := a.c.Do(ctx, "GET", "/v1/servers?hostname="+url.QueryEscape(ref), nil, &out); err != nil {
		return sv, err
	}
	if len(out.Servers) == 0 {
		return sv, &client.APIError{Status: 404, Code: "not_found", Message: fmt.Sprintf("no server %s", ref)}
	}
	return out.Servers[0], nil
}

func (a *app) serverAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <srv_id> <run>",
		Short: "Attach a server to a Run (a running one starts its command now; a stopped one at its next placement)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.doTenantServer(ctxOf(cmd), "POST", tenantServerPath(args[0], "attach"), map[string]string{"runId": args[1]})
		},
	}
}

func (a *app) serverDetachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "detach <srv_id>",
		Short: "Detach a server from its Run (its command stops; the Run is untouched)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.doTenantServer(ctxOf(cmd), "POST", tenantServerPath(args[0], "detach"), nil)
		},
	}
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
	var state, wake string
	var labels []string
	cmd := &cobra.Command{
		Use:     "ls [run [name]]",
		Aliases: []string{"list"},
		Short:   "List the tenant's servers, or a Run's (or show one of its)",
		Args:    cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				q := url.Values{}
				if state != "" {
					q.Set("state", state)
				}
				if wake != "" {
					q.Set("wake", wake)
				}
				for _, l := range labels {
					q.Add("label", l)
				}
				var out struct {
					Servers []server.TenantServer `json:"servers"`
				}
				if err := a.c.Do(ctxOf(cmd), "GET", "/v1/servers?"+q.Encode(), nil, &out); err != nil {
					return err
				}
				if a.output == "json" {
					return a.json(out.Servers)
				}
				rows := make([][]string, 0, len(out.Servers))
				for _, sv := range out.Servers {
					idle := "-"
					if sv.IdleAt != nil {
						idle = time.Until(*sv.IdleAt).Round(time.Second).String()
					}
					rows = append(rows, []string{sv.ID, sv.Name, sv.State, sv.Wake, orDash(strPtr(sv.RunID)), ago(sv.LastRequestAt), idle, orDash(strPtr(sv.URL))})
				}
				a.table("ID\tNAME\tSTATE\tWAKE\tRUN\tLAST REQUEST\tIDLE IN\tURL", rows)
				return nil
			}
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
	cmd.Flags().StringVar(&state, "state", "", "the tenant's servers in these states (comma-separated): ready, waking, asleep, stopped, unreachable, exited, no answer")
	cmd.Flags().StringVar(&wake, "wake", "", "the tenant's servers that wake so: request, never")
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "the tenant's servers with this label, K=V (repeatable)")
	return cmd
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
		Use:   action + " <srv_id> | <run> <name>",
		Short: short,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return a.doTenantServer(ctxOf(cmd), "POST", tenantServerPath(args[0], action), nil)
			}
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
		Use:     "rm <srv_id> | <run> <name>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete a server (detaching it first: its command stops, its URL is gone)",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if err := a.c.Do(ctxOf(cmd), "DELETE", tenantServerPath(args[0]), nil, nil); err != nil {
					return err
				}
				if a.output == "json" {
					return a.json(map[string]any{"deleted": args[0]})
				}
				fmt.Fprintf(a.stdout, "%s deleted\n", args[0])
				return nil
			}
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
