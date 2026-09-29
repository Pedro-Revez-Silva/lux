package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/marcioapm/lux/internal/client"
	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/spec"
)

type Run = server.Run

func (a *app) runCmd() *cobra.Command {
	var file, idem, secretsFrom, pool string
	var follow, wait bool
	var labels []string
	cmd := &cobra.Command{
		Use:   "run -f spec.yaml [-- command...]",
		Short: "Submit a Run",
		Long: `Submit a Run from a spec file (YAML or JSON), or a quick generic Run:

  lux run -f spec.yaml --follow
  lux run --image alpine -- echo hello
  lux run --image alpine --pool arm64 -- uname -m

Flags win over the spec file: --pool replaces placement.pool, as --image
and --name replace theirs. Without --pool or placement.pool, the server
picks the pool.

Secret values in the spec can be read from the environment with
value: ${NAME}, or from a .env file with --secrets-from.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sp spec.RunSpec
			if file != "" {
				b, err := readFileOrStdin(file)
				if err != nil {
					return err
				}
				if err := yaml.Unmarshal(b, &sp); err != nil {
					return fmt.Errorf("%s: %w", file, err)
				}
			}
			img, _ := cmd.Flags().GetString("image")
			if img != "" {
				sp.Image = spec.Image{Ref: img}
			}
			if len(args) > 0 {
				sp.Workload.Command = args
				if sp.Workload.Adapter == "" {
					sp.Workload.Adapter = "generic"
				}
			}
			if name, _ := cmd.Flags().GetString("name"); name != "" {
				sp.Name = name
			}
			if pool != "" {
				sp.Placement.Pool = pool
			}
			for _, l := range labels {
				k, v, _ := strings.Cut(l, "=")
				if sp.Labels == nil {
					sp.Labels = map[string]string{}
				}
				sp.Labels[k] = v
			}
			if err := fillSecrets(sp.Secrets, secretsFrom); err != nil {
				return err
			}
			var run Run
			var hdr []string
			if idem != "" {
				hdr = []string{"Idempotency-Key", idem}
			}
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs", sp, &run, hdr...); err != nil {
				return err
			}
			if !follow && !wait {
				if a.output == "json" {
					return a.json(run)
				}
				fmt.Fprintln(a.stdout, run.ID)
				return nil
			}
			fmt.Fprintln(a.stderr, run.ID)
			if follow {
				if _, err := a.followLogs(ctxOf(cmd), run.ID, "", logOpts{stderr: true}); err != nil {
					return err
				}
			}
			return a.waitExit(ctxOf(cmd), run.ID)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "spec file (YAML or JSON; - for stdin)")
	cmd.Flags().String("image", "", "image ref, for a quick generic Run")
	cmd.Flags().String("name", "", "a name for humans")
	cmd.Flags().StringVar(&pool, "pool", "", "pool to place the Run in (overrides the spec's placement.pool)")
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "label key=value (repeatable)")
	cmd.Flags().StringVar(&idem, "idempotency-key", "", "make the submission safe to retry")
	cmd.Flags().StringVar(&secretsFrom, "secrets-from", "", ".env file supplying secret values")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream output until the Run ends; exit with its code")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the Run ends; exit with its code")
	return cmd
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		var b strings.Builder
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			b.WriteString(sc.Text() + "\n")
		}
		return []byte(b.String()), sc.Err()
	}
	return os.ReadFile(path)
}

// fillSecrets resolves secret values: ${NAME} from the environment, or
// from a .env file for secrets without a value.
func fillSecrets(secrets []spec.Secret, envFile string) error {
	fileVals := map[string]string{}
	if envFile != "" {
		b, err := os.ReadFile(envFile)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			k, v, ok := strings.Cut(strings.TrimPrefix(l, "export "), "=")
			if ok {
				fileVals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	for i := range secrets {
		s := &secrets[i]
		if strings.HasPrefix(s.Value, "${") && strings.HasSuffix(s.Value, "}") {
			name := s.Value[2 : len(s.Value)-1]
			v, ok := os.LookupEnv(name)
			if !ok {
				return fmt.Errorf("secret %s: environment variable %s is not set", s.Name, name)
			}
			s.Value = v
		}
		if s.Value == "" {
			if v, ok := fileVals[s.Name]; ok {
				s.Value = v
			} else if v, ok := os.LookupEnv(s.Name); ok {
				s.Value = v
			}
		}
	}
	return nil
}

func (a *app) lsCmd() *cobra.Command {
	var state, host string
	var labels []string
	var resumable bool
	var limit int
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List Runs",
		Long: `List Runs, newest first. With an operator key, every tenant's
(--tenant narrows it), with a TENANT column.

RUNTIME is the time the Run's placements have spent running, summed (a
placement still running counts up to now); - for a Run that never ran.

COST is the Run's list-price total when it has one currency, "multi" when
it has several (lux cost <run> shows them), and — while nothing has been
reported. A leading ~ marks a total that may still change: part of it is
an estimate, or a cost source has not answered yet.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if state != "" {
				q.Set("state", state)
			}
			if resumable {
				q.Set("resumable", "true")
			}
			if host != "" {
				q.Set("host", host)
			}
			if limit > 0 {
				q.Set("limit", fmt.Sprint(limit))
			}
			for _, l := range labels {
				q.Add("label", l)
			}
			var resp struct {
				Runs []Run `json:"runs"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs?"+q.Encode(), nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Runs)
			}
			tenants := map[string]bool{}
			for _, r := range resp.Runs {
				tenants[r.Tenant] = true
			}
			var rows [][]string
			for _, r := range resp.Runs {
				state := r.State
				if r.Activity == "idle" && r.State == "running" {
					state += " (waiting for input)"
				}
				row := []string{r.ID, orDash(r.Name), state, orDash(r.Host), orDash(r.Pool), r.Spec.Workload.Adapter, runtimeCell(r), runCostCell(r.Cost), ago(&r.CreatedAt)}
				if len(tenants) > 1 {
					row = append([]string{r.Tenant}, row...)
				}
				rows = append(rows, row)
			}
			header := "ID\tNAME\tSTATE\tHOST\tPOOL\tADAPTER\tRUNTIME\tCOST\tCREATED"
			if len(tenants) > 1 {
				header = "TENANT\t" + header
			}
			a.table(header, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state (comma-separated)")
	cmd.Flags().BoolVar(&resumable, "resumable", false, "only Runs resume accepts (stopped, lost, failed; not those whose only snapshot report was refused)")
	cmd.Flags().StringVar(&host, "host", "", "only Runs placed on this host (id or name), ever")
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many (default 100, max 1000)")
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "filter by label key=value")
	return cmd
}

// runtimeCell is the Runs list's RUNTIME column: runtimeSeconds in its two
// largest units ("45s", "3m20s", "2h5m", "1d3h"), "-" for a Run that never ran.
func runtimeCell(r Run) string {
	if r.RuntimeSince == nil && r.RuntimeSeconds == 0 {
		return "-"
	}
	s := int64(r.RuntimeSeconds)
	units := []struct {
		name string
		size int64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}
	for i, u := range units[:3] {
		if s >= u.size {
			out := fmt.Sprintf("%d%s", s/u.size, u.name)
			if rest := s % u.size / units[i+1].size; rest > 0 {
				out += fmt.Sprintf("%d%s", rest, units[i+1].name)
			}
			return out
		}
	}
	return fmt.Sprintf("%ds", s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) getRun(ctx context.Context, id string) (*Run, error) {
	var run Run
	if err := a.c.Do(ctx, "GET", "/v1/runs/"+id, nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (a *app) getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <run>",
		Short: "Show a Run, its placements and resource usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := a.getRun(ctxOf(cmd), args[0])
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(run)
			}
			w := a.stdout
			fmt.Fprintf(w, "id:        %s\n", run.ID)
			fmt.Fprintf(w, "tenant:    %s\n", run.Tenant)
			if run.Name != "" {
				fmt.Fprintf(w, "name:      %s\n", run.Name)
			}
			fmt.Fprintf(w, "state:     %s", run.State)
			if run.StateReason != "" {
				fmt.Fprintf(w, " (%s)", run.StateReason)
			}
			if run.Activity != "" {
				fmt.Fprintf(w, " [%s]", run.Activity)
			}
			fmt.Fprintln(w)
			if run.ExitCode != nil {
				fmt.Fprintf(w, "exit code: %d\n", *run.ExitCode)
			}
			fmt.Fprintf(w, "adapter:   %s\n", run.Spec.Workload.Adapter)
			if run.Spec.Image.Ref != "" {
				fmt.Fprintf(w, "image:     %s\n", run.Spec.Image.Ref)
			} else if run.Image != nil {
				fmt.Fprintf(w, "image:     built %s\n", run.Image.ImageID)
				for _, l := range strings.Split(run.Image.Containerfile, "\n") {
					if f, ok := spec.ParseFrom(l); ok {
						fmt.Fprintf(w, "           %s\n", f.With(f.Image))
					}
				}
			} else {
				fmt.Fprintln(w, "image:     built (not yet)")
			}
			if run.SessionID != "" {
				fmt.Fprintf(w, "session:   %s\n", run.SessionID)
			}
			fmt.Fprintf(w, "created:   %s\n", run.CreatedAt.Format(time.RFC3339))
			if len(run.Placements) > 0 {
				fmt.Fprintln(w, "placements:")
				for _, p := range run.Placements {
					code := "-"
					if p.ExitCode != nil {
						code = fmt.Sprint(*p.ExitCode)
					}
					fmt.Fprintf(w, "  %d  %-12s %-8s exit=%s %s\n", p.Epoch, p.HostName, p.State, code, p.ExitReason)
				}
			}
			if len(run.Servers) > 0 {
				fmt.Fprintln(w, "servers:")
				for _, sv := range run.Servers {
					fmt.Fprintf(w, "  %-12s %-6d %-24s %s\n", sv.Name, sv.Port, serverState(sv), serverURL(sv))
				}
			}
			if u := run.Usage; u != nil && u.Placements > 0 {
				fmt.Fprintf(w, "usage:     peak memory %s, peak disk %s, cpu %.1fs\n",
					bytesHuman(u.PeakMemoryBytes), bytesHuman(u.PeakDiskBytes), u.CPUSeconds)
			}
			if rs := run.Resume; rs != nil {
				from := "from scratch"
				if rs.Snapshot != nil {
					from = "from " + *rs.Snapshot
					if len(rs.OnHosts) > 0 {
						from += " (on " + strings.Join(rs.OnHosts, ", ") + ")"
					}
					if !rs.Uploaded {
						from += ", not uploaded"
					}
				}
				fmt.Fprintf(w, "resume:    %s\n", from)
				if len(rs.Secrets) > 0 {
					held := "must be supplied again"
					if rs.SecretsHeld {
						held = "held by luxd (an operator may resume without them)"
					}
					fmt.Fprintf(w, "secrets:   %s: %s\n", strings.Join(rs.Secrets, ", "), held)
				}
				for _, b := range rs.Blockers {
					fmt.Fprintf(w, "blocked:   %s\n", b)
				}
			}
			return nil
		},
	}
}

type logOpts struct {
	stderr, events bool
	// server: only this server's output; servers: servers' output too.
	server  string
	servers bool
}

func (a *app) logsCmd() *cobra.Command {
	var follow bool
	var since string
	var o logOpts
	cmd := &cobra.Command{
		Use:   "logs <run>",
		Short: "Print a Run's output",
		Long:  "Print a Run's output, across every placement, from wherever it lives.\nWith -o json, prints one JSON record per line (cursor, channel, data).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if follow {
				_, err := a.followLogs(ctxOf(cmd), args[0], since, o)
				return err
			}
			_, err := a.printLogs(ctxOf(cmd), args[0], since, false, o)
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the Run stops or ends")
	cmd.Flags().StringVar(&since, "since", "", "resume from a cursor")
	cmd.Flags().BoolVar(&o.stderr, "stderr", true, "include stderr")
	cmd.Flags().BoolVar(&o.events, "events", false, "include structured events and lifecycle")
	cmd.Flags().StringVar(&o.server, "server", "", "only this server's output")
	cmd.Flags().BoolVar(&o.servers, "servers", false, "include the Run's servers' output (prefixed with their names)")
	return cmd
}

// followLogs streams until the Run stops or ends, reconnecting if the
// stream drops. Returns the last cursor.
func (a *app) followLogs(ctx context.Context, id, since string, o logOpts) (string, error) {
	cur := since
	for {
		c, err := a.printLogs(ctx, id, cur, true, o)
		if c != "" {
			cur = c
		}
		if err == nil || ctx.Err() != nil {
			return cur, err
		}
		var ae *client.APIError
		if errors.As(err, &ae) {
			return cur, err
		}
		time.Sleep(time.Second)
	}
}

func (a *app) printLogs(ctx context.Context, id, since string, follow bool, o logOpts) (string, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if follow {
		q.Set("follow", "true")
	}
	if o.events {
		q.Set("events", "true")
	}
	if o.server != "" {
		q.Set("server", o.server)
	}
	if o.servers {
		q.Set("servers", "true")
	}
	cur := since
	ended := false
	err := a.c.Stream(ctx, "/v1/runs/"+id+"/output", q, func(ev client.SSEEvent) error {
		switch ev.Event {
		case "record":
			var r server.OutputRecord
			if err := json.Unmarshal(ev.Data, &r); err != nil {
				return nil
			}
			cur = r.Cursor
			if a.output == "json" {
				if r.Ch == "event" && !o.events {
					return nil
				}
				return json.NewEncoder(a.stdout).Encode(r)
			}
			switch r.Ch {
			case "stdout":
				fmt.Fprint(a.stdout, r.Data)
			case "stderr":
				if o.stderr {
					fmt.Fprint(a.stderr, r.Data)
				}
			case "server":
				w := a.stdout
				if r.Stream == "stderr" {
					if !o.stderr {
						return nil
					}
					w = a.stderr
				}
				switch {
				case r.Data == "":
					if o.events {
						fmt.Fprintf(a.stderr, "· [%s] %s\n", r.Server, r.Event)
					}
				case o.server != "":
					fmt.Fprint(w, r.Data)
				default:
					for _, l := range strings.SplitAfter(r.Data, "\n") {
						if l != "" {
							fmt.Fprintf(w, "[%s] %s", r.Server, l)
						}
					}
				}
			case "event":
				if o.events {
					fmt.Fprintf(a.stderr, "· %s\n", r.Event)
				}
			}
		case "lux":
			if a.output == "json" {
				fmt.Fprintf(a.stdout, "{\"lux\":%s}\n", ev.Data)
			} else {
				var e server.Event
				_ = json.Unmarshal(ev.Data, &e)
				fmt.Fprintf(a.stderr, "── %s %v\n", e.Type, e.Data)
			}
		case "gap":
			var g map[string]any
			_ = json.Unmarshal(ev.Data, &g)
			fmt.Fprintf(a.stderr, "── output missing for placement %v: %v\n", g["epoch"], g["reason"])
		case "end":
			var e struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(ev.Data, &e)
			if e.Cursor != "" {
				cur = e.Cursor
			}
			ended = true
		case "error":
			return fmt.Errorf("%s", ev.Data)
		}
		return nil
	})
	if err == nil && !ended {
		err = errors.New("output stream ended early")
	}
	return cur, err
}

func (a *app) eventsCmd() *cobra.Command {
	var all, follow bool
	var after int64
	cmd := &cobra.Command{
		Use:   "events <run> | --all",
		Short: "Print a Run's lifecycle events, or every Run's",
		Long: `Print a Run's lifecycle events. With --all, the events of every Run you
can see (an operator: every tenant's; --tenant narrows it) as they happen,
from now or from --after an event id; --follow=false prints what there is.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return fmt.Errorf("give a run, or --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return a.feed(ctxOf(cmd), after, follow)
			}
			var resp struct {
				Events []server.Event `json:"events"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/events", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Events)
			}
			for _, e := range resp.Events {
				d, _ := json.Marshal(e.Data)
				ep := "-"
				if e.Epoch != nil {
					ep = fmt.Sprint(*e.Epoch)
				}
				fmt.Fprintf(a.stdout, "%s  %-3s %-20s %s\n", e.Time.Format("15:04:05.000"), ep, e.Type, d)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "every Run's events")
	cmd.Flags().BoolVar(&follow, "follow", true, "with --all: keep streaming")
	cmd.Flags().Int64Var(&after, "after", -1, "with --all: from after this event id (default: from now)")
	return cmd
}

// feed prints GET /v1/events: one line per event (JSON lines with -o json).
func (a *app) feed(ctx context.Context, after int64, follow bool) error {
	q := url.Values{"follow": {fmt.Sprint(follow)}}
	if after >= 0 {
		q.Set("after", fmt.Sprint(after))
	}
	return a.c.Stream(ctx, "/v1/events", q, func(ev client.SSEEvent) error {
		if ev.Event != "lux" {
			if ev.Event == "error" {
				return fmt.Errorf("event stream: %s", ev.Data)
			}
			return nil
		}
		if a.output == "json" {
			_, err := fmt.Fprintf(a.stdout, "%s\n", ev.Data)
			return err
		}
		var e server.FeedEvent
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return err
		}
		d, _ := json.Marshal(e.Data)
		_, err := fmt.Fprintf(a.stdout, "%s  %-10s %s  %-20s %s\n", e.Time.Local().Format("15:04:05.000"), e.Tenant, e.RunID, e.Type, d)
		return err
	})
}

func (a *app) migrateCmd() *cobra.Command {
	var to, input string
	var wait bool
	cmd := &cobra.Command{
		Use:   "migrate <run>",
		Short: "Move a running Run to another host (operators)",
		Long: `Stop a running Run, snapshotting its state, and resume it at once on
another host: the one --to names, or any but the one it is on. An agent
resumes its session where it was; --input is delivered once it is running
again (if it was mid-turn, that turn was interrupted: tell it to go on).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := map[string]any{}
			if to != "" {
				req["to"] = to
			}
			if input != "" {
				req["input"] = map[string]string{"text": input}
			}
			var run Run
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/migrate", req, &run); err != nil {
				return err
			}
			if wait {
				from := run.Epoch
				r, err := a.waitMoved(ctxOf(cmd), args[0], from)
				if err != nil {
					return err
				}
				run = *r
			}
			if a.output == "json" {
				return a.json(run)
			}
			fmt.Fprintf(a.stdout, "%s %s %s\n", run.ID, run.State, run.Host)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the host to move to (id or name); default: any other")
	cmd.Flags().StringVar(&input, "input", "", "message to deliver once it is running again")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until it is running on its new host")
	return cmd
}

// waitMoved waits for a Run to run again in a placement after epoch (or to
// end up somewhere it will not: stopped, lost, finished).
func (a *app) waitMoved(ctx context.Context, id string, epoch int) (*Run, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return a.waitFor(ctx, id, func(run *Run) (bool, error) {
		switch {
		case run.State == server.StateRunning && run.Epoch > epoch:
			return true, nil
		case run.State == server.StateStopped && run.Epoch > epoch, run.State == server.StateLost, server.Terminal(run.State):
			return true, fmt.Errorf("run is %s: %s", run.State, run.StateReason)
		}
		return false, nil
	})
}

func (a *app) steerCmd() *cobra.Command {
	var interrupt bool
	var reqID string
	cmd := &cobra.Command{
		Use:   "steer <run> <message>",
		Short: "Send a message to a running workload",
		Long:  "Send input to a running Run. Agents get it as a message (queued until the\ncurrent turn ends if the agent cannot take it mid-turn); generic workloads\nget it on stdin. --interrupt stops the current turn first.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp map[string]string
			in := map[string]any{"text": args[1], "interrupt": interrupt}
			if reqID != "" {
				in["requestId"] = reqID
			}
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/input", in, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp)
			}
			fmt.Fprintln(a.stdout, resp["requestId"])
			return nil
		},
	}
	cmd.Flags().BoolVar(&interrupt, "interrupt", false, "stop the current turn first")
	cmd.Flags().StringVar(&reqID, "request-id", "", "id that makes a retry safe")
	return cmd
}

func (a *app) interruptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "interrupt <run>",
		Short: "Interrupt the workload's current turn",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/input", map[string]any{"interrupt": true}, nil)
		},
	}
}

func (a *app) lifecycle(use, short, path string, wantStates ...string) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   use + " <run>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var run Run
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/"+path, nil, &run); err != nil {
				return err
			}
			if wait {
				r, err := a.waitState(ctxOf(cmd), args[0], wantStates...)
				if err != nil {
					return err
				}
				run = *r
			}
			if a.output == "json" {
				return a.json(run)
			}
			fmt.Fprintf(a.stdout, "%s %s\n", run.ID, run.State)
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until it has taken effect")
	return cmd
}

func (a *app) stopCmd() *cobra.Command {
	return a.lifecycle("stop", "Stop a Run gracefully (resumable)", "stop", "stopped", "succeeded", "failed", "cancelled", "lost")
}

func (a *app) cancelCmd() *cobra.Command {
	return a.lifecycle("cancel", "Stop a Run and make it final", "cancel", "cancelled", "succeeded", "failed")
}

// parseAddRepo reads --add-repo: name=url[@ref][,ref=REF][,credential=SECRET][,path=/abs][,push=false].
// An @ is a ref only after the URL's last / and : (git@github.com:o/r.git
// has none); ref= says it outright. A comma starts an option only before
// one of those keys, so the URL may hold other commas. Options are taken
// off the end, last first.
func parseAddRepo(v string) (spec.Repository, error) {
	var r spec.Repository
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return r, fmt.Errorf("--add-repo %q: want name=url[@ref][,ref=REF][,credential=SECRET][,path=/abs][,push=false]", v)
	}
	r.Name = name
	var opts []string
	for {
		i := -1
		for _, k := range []string{",ref=", ",credential=", ",path=", ",push="} {
			i = max(i, strings.LastIndex(rest, k))
		}
		if i < 0 {
			break
		}
		opts = append(opts, rest[i+1:])
		rest = rest[:i]
	}
	r.URL = rest
	if at := strings.LastIndex(rest, "@"); at > strings.LastIndex(rest, "/") && at > strings.LastIndex(rest, ":") {
		r.URL, r.Ref = rest[:at], rest[at+1:]
	}
	for _, o := range opts {
		k, val, _ := strings.Cut(o, "=")
		switch k {
		case "ref":
			if r.Ref != "" {
				return r, fmt.Errorf("--add-repo %s: the ref is given twice (@ and ref=)", name)
			}
			r.Ref = val
		case "credential":
			r.Credential = val
		case "path":
			r.Path = val
		case "push":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return r, fmt.Errorf("--add-repo %s: push=%s: want true or false", name, val)
			}
			r.Push = &b
		}
	}
	if r.URL == "" {
		return r, fmt.Errorf("--add-repo %s: no url", name)
	}
	return r, nil
}

func (a *app) resumeCmd() *cobra.Command {
	var input, secretsFrom, fromSnapshot, to, disk, reqID string
	var secretArgs, addRepos []string
	var follow, wait bool
	cmd := &cobra.Command{
		Use:   "resume <run>",
		Short: "Resume a stopped, lost or failed Run on any host",
		Long: `Resume a Run. Its secrets must be supplied again (lux never stores them):
from the environment (by name), a .env file (--secrets-from), or --secret NAME=VALUE.

With an operator key, secrets may be left out while luxd still holds them
(it has not restarted since they were supplied), and --to chooses the host.

--add-repo adds a repository, cloned into the restored workspace before the
Run starts again (repeatable):

  --add-repo name=url[@ref][,ref=REF][,credential=SECRET][,path=/abs][,push=false]

An @ after the URL's last / names the ref (https://host/o/r.git@main);
git@host:o/r.git has none. ref=REF says it outright. A credential's value
is found like the other secrets'. If the clone fails the Run goes on
without it. The request id (--request-id, or generated) is printed on
stderr as "request <id>", and marks the repository's git.clone event.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			var repos []spec.Repository
			for _, v := range addRepos {
				r, err := parseAddRepo(v)
				if err != nil {
					return err
				}
				repos = append(repos, r)
			}
			run, err := a.getRun(ctx, args[0])
			if err != nil {
				return err
			}
			var secrets []spec.Secret
			for _, ref := range run.Secrets {
				secrets = append(secrets, spec.Secret{Name: ref.Name})
			}
			// Added repositories' credentials are looked for like the rest.
			for _, r := range repos {
				if c := r.Credential; c != "" && !slices.ContainsFunc(secrets, func(s spec.Secret) bool { return s.Name == c }) {
					secrets = append(secrets, spec.Secret{Name: c})
				}
			}
			for _, kv := range secretArgs {
				k, v, _ := strings.Cut(kv, "=")
				for i := range secrets {
					if secrets[i].Name == k {
						secrets[i].Value = v
					}
				}
			}
			if err := fillSecrets(secrets, secretsFrom); err != nil {
				return err
			}
			// Only what was supplied: with none, an operator's resume uses
			// the values luxd still holds.
			secrets = slices.DeleteFunc(secrets, func(s spec.Secret) bool { return s.Value == "" })
			req := map[string]any{"secrets": secrets}
			if input != "" {
				req["input"] = map[string]string{"text": input}
			}
			if fromSnapshot != "" {
				req["fromSnapshot"] = fromSnapshot
			}
			if to != "" {
				req["to"] = to
			}
			if disk != "" {
				req["resources"] = map[string]string{"disk": disk}
			}
			if reqID != "" {
				req["requestId"] = reqID
			}
			if len(repos) > 0 {
				req["git"] = map[string]any{"repositories": repos}
			}
			var out Run
			hdr, err := a.c.DoHeader(ctx, "POST", "/v1/runs/"+args[0]+"/resume", req, &out)
			if err != nil {
				return err
			}
			if id := hdr.Get("Lux-Request-Id"); id != "" {
				fmt.Fprintln(a.stderr, "request", id)
			}
			if follow {
				if _, err := a.followLogs(ctx, args[0], "", logOpts{stderr: true}); err != nil {
					return err
				}
				return a.waitExit(ctx, args[0])
			}
			if wait {
				r, err := a.waitState(ctx, args[0], "running", "succeeded", "failed", "cancelled", "stopped", "lost")
				if err != nil {
					return err
				}
				out = *r
			}
			if a.output == "json" {
				return a.json(out)
			}
			fmt.Fprintf(a.stdout, "%s %s\n", out.ID, out.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "message to deliver once it is running")
	cmd.Flags().StringVar(&secretsFrom, "secrets-from", "", ".env file supplying secret values")
	cmd.Flags().StringArrayVar(&secretArgs, "secret", nil, "NAME=VALUE (repeatable)")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "resume from an older snapshot")
	cmd.Flags().StringVar(&to, "to", "", "operators: resume on this host (id or name)")
	cmd.Flags().StringVar(&disk, "disk", "", "a new disk limit from now on (e.g. 40Gi), for a Run that went over")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream output until it ends")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until it is running (or has ended)")
	cmd.Flags().StringArrayVar(&addRepos, "add-repo", nil, "add a repository: name=url[@ref][,ref=REF][,credential=SECRET][,path=/abs][,push=false] (repeatable)")
	cmd.Flags().StringVar(&reqID, "request-id", "", "names this resume (in its events and added repositories); generated if absent")
	return cmd
}

func (a *app) waitCmd() *cobra.Command {
	var states []string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <run>",
		Short: "Wait for a Run to reach a state (default: until it ends); exit with its code",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			if len(states) == 0 {
				return a.waitExit(ctx, args[0])
			}
			run, err := a.waitState(ctx, args[0], states...)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(run)
			}
			fmt.Fprintf(a.stdout, "%s %s\n", run.ID, run.State)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", nil, "states to wait for (comma-separated)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long")
	return cmd
}

func (a *app) waitState(ctx context.Context, id string, states ...string) (*Run, error) {
	return a.waitFor(ctx, id, func(run *Run) (bool, error) { return slices.Contains(states, run.State), nil })
}

// waitFor polls a Run until done says so (or fails), or ctx ends.
func (a *app) waitFor(ctx context.Context, id string, done func(*Run) (bool, error)) (*Run, error) {
	for {
		run, err := a.getRun(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok, err := done(run); ok || err != nil {
			return run, err
		}
		select {
		case <-ctx.Done():
			return run, fmt.Errorf("timed out waiting: run is %s", run.State)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// waitExit waits for a Run to stop or end and returns its exit code as the
// command's.
func (a *app) waitExit(ctx context.Context, id string) error {
	run, err := a.waitState(ctx, id, "succeeded", "failed", "cancelled", "stopped", "lost")
	if err != nil {
		return err
	}
	if a.output == "json" {
		_ = a.json(run)
	} else if run.State != "succeeded" {
		msg := run.State
		if run.StateReason != "" {
			msg += ": " + run.StateReason
		}
		fmt.Fprintln(a.stderr, "lux:", id, msg)
	}
	switch {
	case run.State == "succeeded":
		return nil
	case run.ExitCode != nil && *run.ExitCode != 0:
		return exitCode(*run.ExitCode)
	}
	return exitCode(1)
}
