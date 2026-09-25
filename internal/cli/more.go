package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/marcioapm/lux/internal/server"
)

func (a *app) pushCmd() *cobra.Command {
	var wait bool
	var expect []string
	cmd := &cobra.Command{
		Use:   "push <run>",
		Short: "Push the Run's repositories to its git.push branch",
		Long: `Push each repository's current commit to the spec's git.push branch,
with credentials only the runner holds. A branch that moved since this Run
last pushed it is not overwritten. --expect repo=sha instead pushes only if
the branch is at that commit (for pushing onto a branch someone else owns).
--wait prints each repository's outcome and fails unless every one was
pushed or already up to date.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if len(expect) > 0 {
				ex := map[string]string{}
				for _, kv := range expect {
					repo, sha, ok := strings.Cut(kv, "=")
					repo, sha = strings.TrimSpace(repo), strings.ToLower(strings.TrimSpace(sha))
					if !ok || repo == "" || sha == "" {
						return fmt.Errorf("--expect %q: want repo=sha", kv)
					}
					if _, dup := ex[repo]; dup {
						return fmt.Errorf("--expect: %s given twice", repo)
					}
					ex[repo] = sha
				}
				body["expect"] = ex
			}
			var resp map[string]string
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/push", body, &resp); err != nil {
				return err
			}
			if !wait {
				if a.output == "json" {
					return a.json(resp)
				}
				fmt.Fprintln(a.stdout, resp["requestId"])
				return nil
			}
			return a.waitPush(cmd, args[0], resp["requestId"])
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the outcome")
	cmd.Flags().StringArrayVar(&expect, "expect", nil, "repo=sha: push only if the branch is at that commit (repeatable)")
	return cmd
}

// waitPush waits for the push's git.push event (one per request, with
// every repository's result), reading events from a cursor.
func (a *app) waitPush(cmd *cobra.Command, runID, requestID string) error {
	ctx, cancel := context.WithTimeout(ctxOf(cmd), 15*time.Minute)
	defer cancel()
	var after int64
	for {
		var resp struct {
			Events []server.Event `json:"events"`
		}
		if err := a.c.Do(ctx, "GET", fmt.Sprintf("/v1/runs/%s/events?after=%d", runID, after), nil, &resp); err != nil {
			return err
		}
		for _, e := range resp.Events {
			after = e.ID
			if e.Type != "git.push" || e.Data["requestId"] != requestID {
				continue
			}
			results, _ := e.Data["results"].([]any)
			if a.output == "json" {
				_ = a.json(results)
			}
			failed := false
			for _, x := range results {
				r, _ := x.(map[string]any)
				if a.output != "json" {
					fmt.Fprintf(a.stdout, "%s → %s: %s %s\n", r["repo"], r["branch"], r["status"], r["error"])
				}
				if r["status"] != "pushed" && r["status"] != "up-to-date" && r["status"] != "skipped" {
					failed = true
				}
			}
			if failed {
				return exitCode(1)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for the push (is the Run still running?)")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (a *app) snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots <run>",
		Short: "List a Run's snapshots and where each lives",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Snapshots []server.Snapshot `json:"snapshots"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/snapshots", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Snapshots)
			}
			var rows [][]string
			for _, s := range resp.Snapshots {
				var size int64
				for _, v := range s.Manifest.Volumes {
					size += v.Size
				}
				where := []string{}
				if s.OnHost != "" {
					where = append(where, "host "+s.OnHost)
				}
				if s.Uploaded {
					where = append(where, "s3")
				}
				if !s.Available {
					where = []string{"unavailable"}
				}
				rows = append(rows, []string{s.ID, fmt.Sprint(s.Epoch), bytesHuman(size), strings.Join(where, ", "), ago(&s.CreatedAt)})
			}
			a.table("ID\tEPOCH\tSIZE\tWHERE\tCREATED", rows)
			return nil
		},
	}
}

func (a *app) artifactsCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "artifacts <run>",
		Short: "List (or download) the files a Run produced",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Artifacts []server.Artifact `json:"artifacts"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/artifacts", nil, &resp); err != nil {
				return err
			}
			if dir != "" {
				// Every path checked before any download starts, so a
				// rejected one leaves nothing behind.
				dsts := make([]string, len(resp.Artifacts))
				for i, art := range resp.Artifacts {
					dst, err := downloadPath(dir, art.Epoch, art.Path)
					if err != nil {
						return err
					}
					dsts[i] = dst
				}
				// A few at a time: each is a round trip through luxd. One
				// failing cancels the rest (their temp files are removed).
				g, ctx := errgroup.WithContext(ctxOf(cmd))
				g.SetLimit(4)
				var mu sync.Mutex
				for i, art := range resp.Artifacts {
					dst := dsts[i]
					g.Go(func() error {
						if err := a.download(ctx, art.ID, dst); err != nil {
							return fmt.Errorf("%s: %w", art.Path, err)
						}
						if a.output != "json" {
							mu.Lock()
							fmt.Fprintln(a.stdout, dst)
							mu.Unlock()
						}
						return nil
					})
				}
				if err := g.Wait(); err != nil {
					return err
				}
			}
			if a.output == "json" {
				return a.json(resp.Artifacts)
			}
			if dir == "" {
				var rows [][]string
				for _, art := range resp.Artifacts {
					rows = append(rows, []string{art.ID, fmt.Sprint(art.Epoch), art.Path, bytesHuman(art.Size), art.ContentType})
				}
				a.table("ID\tEPOCH\tPATH\tSIZE\tTYPE", rows)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "download", "", "download every artifact into this directory")
	return cmd
}

// downloadPath is where an artifact goes under dir: <epoch>/<its path>.
// The path is the Run's to choose, so it must stay inside dir.
func downloadPath(dir string, epoch int, artPath string) (string, error) {
	rel := filepath.FromSlash(strings.TrimPrefix(artPath, "/"))
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s: path outside the download directory", artPath)
	}
	return filepath.Join(dir, fmt.Sprint(epoch), rel), nil
}

// download fetches an artifact (luxd streams it) into dst, atomically.
func (a *app) download(ctx context.Context, id, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", a.c.Base+"/v1/artifacts/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.c.Key)
	resp, err := a.c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".lux-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		f.Close()
		return err
	}
	// Whole, and the file the Run wrote: or not written at all.
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		f.Close()
		return fmt.Errorf("download cut short: %d of %d bytes", n, resp.ContentLength)
	}
	if want := resp.Header.Get("X-Lux-SHA256"); want != "" && want != hex.EncodeToString(h.Sum(nil)) {
		f.Close()
		return errors.New("download does not match the artifact's sha256")
	}
	// Like os.Create would make it (CreateTemp's are private).
	if err := f.Chmod(0o666 &^ umask()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

func (a *app) hostsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hosts", Short: "Hosts that run your workloads"}
	var all bool
	var pool, state string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List hosts",
		Long: `List hosts: your tenant's and the platform's. With an operator key,
every host, with a TENANT column (--tenant: what that tenant sees).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Hosts []server.Host `json:"hosts"`
			}
			q := url.Values{}
			if all {
				q.Set("all", "true")
			}
			if pool != "" {
				q.Set("pool", pool)
			}
			if state != "" {
				q.Set("state", state)
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/hosts?"+q.Encode(), nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Hosts)
			}
			tenants := false
			for _, h := range resp.Hosts {
				tenants = tenants || h.Tenant != ""
			}
			var rows [][]string
			for _, h := range resp.Hosts {
				row := []string{h.Name, h.Pool, hostState(h), fmt.Sprint(h.LiveRuns), fmt.Sprintf("%g/%g", h.Allocated.CPUs, h.Capacity.CPUs),
					bytesHuman(int64(h.Allocated.Memory)) + "/" + bytesHuman(h.Capacity.Memory), ago(h.LastHeartbeat)}
				if tenants {
					row = append([]string{h.Name, cmp.Or(h.Tenant, "(platform)")}, row[1:]...)
				}
				rows = append(rows, row)
			}
			header := "NAME\tPOOL\tSTATE\tRUNS\tCPUS\tMEMORY\tHEARTBEAT"
			if tenants {
				header = "NAME\tTENANT\tPOOL\tSTATE\tRUNS\tCPUS\tMEMORY\tHEARTBEAT"
			}
			a.table(header, rows)
			return nil
		},
	}
	ls.Flags().BoolVar(&all, "all", false, "include terminated hosts")
	ls.Flags().StringVar(&pool, "pool", "", "only this pool's hosts")
	ls.Flags().StringVar(&state, "state", "", "only hosts in this state (provisioning, ready, draining, lost, terminated)")
	get := &cobra.Command{
		Use:   "get <host>",
		Short: "Show a host (by id or name), its lifecycle and its live Runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var h server.Host
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/hosts/"+args[0], nil, &h); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(h)
			}
			w := a.stdout
			fmt.Fprintf(w, "id:        %s\n", h.ID)
			fmt.Fprintf(w, "name:      %s\n", h.Name)
			fmt.Fprintf(w, "tenant:    %s\n", cmp.Or(h.Tenant, "(platform)"))
			fmt.Fprintf(w, "pool:      %s\n", h.Pool)
			fmt.Fprintf(w, "state:     %s", hostState(h))
			if h.StateReason != "" {
				fmt.Fprintf(w, " (%s)", h.StateReason)
			}
			fmt.Fprintln(w)
			if h.ProviderID != nil {
				fmt.Fprintf(w, "provider:  %s\n", *h.ProviderID)
			}
			fmt.Fprintf(w, "capacity:  %g cpus, %s memory, %s disk, %d runs\n", h.Capacity.CPUs, bytesHuman(h.Capacity.Memory), bytesHuman(h.Capacity.Disk), h.Capacity.Runs)
			fmt.Fprintf(w, "allocated: %g cpus, %s memory\n", h.Allocated.CPUs, bytesHuman(int64(h.Allocated.Memory)))
			fmt.Fprintf(w, "heartbeat: %s\n", ago(h.LastHeartbeat))
			fmt.Fprintln(w, "lifecycle:")
			for _, k := range []string{"provisionRequested", "provisioned", "registered", "firstPlacement", "lastPlacementEnded",
				"drainRequested", "terminateRequested", "terminated", "lost"} {
				if t := h.Times[k]; t != nil {
					fmt.Fprintf(w, "  %-19s %s\n", k, t.Format(time.RFC3339))
				}
			}
			if len(h.Placements) > 0 {
				fmt.Fprintln(w, "runs:")
				for _, p := range h.Placements {
					fmt.Fprintf(w, "  %s  %-10s %-9s %s  %g cpus, %s  since %s\n", p.RunID, orDash(p.RunName), p.State, p.Tenant,
						p.Resources.CPUs, bytesHuman(int64(p.Resources.Memory)), ago(&p.Since))
				}
			}
			return nil
		},
	}
	var forceEvict bool
	drain := &cobra.Command{
		Use:   "drain <host>",
		Short: "Stop placing new Runs on a host",
		Long: `Stop placing new Runs on a host. Its live Runs finish where they are;
--force-evict stops them too, so they resume elsewhere.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"forceEvict": forceEvict}
			return a.c.Do(ctxOf(cmd), "POST", "/v1/hosts/"+args[0]+"/drain", body, nil)
		},
	}
	drain.Flags().BoolVar(&forceEvict, "force-evict", false, "also stop the host's live Runs so they resume elsewhere")
	cmd.AddCommand(ls, get, drain)
	return cmd
}

func hostState(h server.Host) string {
	if h.Draining && h.State != "draining" {
		return h.State + " (draining)"
	}
	return h.State
}

func (a *app) tenantsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tenants", Short: "Tenants (operator keys only)"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List tenants, their quotas and what they use",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Tenants []server.Tenant `json:"tenants"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/tenants", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Tenants)
			}
			limit := func(n *int) string {
				if n == nil {
					return "-"
				}
				return fmt.Sprint(*n)
			}
			var rows [][]string
			for _, t := range resp.Tenants {
				stored := bytesHuman(t.StoredBytes)
				if t.MaxStorageBytes != nil {
					stored += "/" + bytesHuman(*t.MaxStorageBytes)
				}
				rows = append(rows, []string{t.Name, t.ID, fmt.Sprintf("%d/%s", t.ActiveRuns, limit(t.MaxConcurrentRuns)), fmt.Sprint(t.Runs),
					fmt.Sprintf("%d/%s", t.Hosts, limit(t.MaxHosts)), stored, fmt.Sprintf("%dd", t.RetentionDays)})
			}
			a.table("NAME\tID\tACTIVE\tRUNS\tHOSTS\tSTORED\tRETENTION", rows)
			return nil
		},
	}
	cmd.AddCommand(ls)
	return cmd
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Runs, queue, hosts and capacity now",
		Long: `The state of what you can see now: Runs by state, the queue and how long
Runs waited to start, hosts by state, and capacity against what live
placements hold. With an operator key, the whole system (--tenant narrows it).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var st server.Status
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/status", nil, &st); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(st)
			}
			w := a.stdout
			fmt.Fprintf(w, "runs:      %s\n", counts(st.Runs))
			fmt.Fprintf(w, "running:   %d busy, %d waiting for input\n", st.Busy, st.Idle)
			fmt.Fprintf(w, "queued:    %d", st.Queued)
			if st.OldestQueuedAt != nil {
				fmt.Fprintf(w, " (oldest since %s)", ago(st.OldestQueuedAt))
			}
			fmt.Fprintln(w)
			if l := st.StartLatency; l.N > 0 {
				fmt.Fprintf(w, "to start:  p50 %.1fs, p95 %.1fs, max %.1fs (%d in the last hour)\n", *l.P50, *l.P95, *l.Max, l.N)
			}
			fmt.Fprintf(w, "hosts:     %s\n", counts(st.Hosts))
			c, u := st.Capacity, st.Allocated
			fmt.Fprintf(w, "allocated: %g/%g cpus, %s/%s memory, %d/%d runs\n", u.CPUs, c.CPUs,
				bytesHuman(u.Memory), bytesHuman(c.Memory), u.Runs, c.Runs)
			return nil
		},
	}
}

// counts prints "3 running, 1 stopped", largest first.
func counts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", m[k], k)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func (a *app) poolsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pools", Short: "Host pools"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List pools",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Pools []server.Pool `json:"pools"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/pools", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Pools)
			}
			var rows [][]string
			for _, p := range resp.Pools {
				rows = append(rows, []string{p.Name, p.Provider, fmt.Sprintf("%d-%d", p.MinHosts, p.MaxHosts), fmt.Sprint(p.WarmHosts), fmt.Sprint(p.Shared)})
			}
			a.table("NAME\tPROVIDER\tHOSTS\tWARM\tSHARED", rows)
			return nil
		},
	}
	var p server.Pool
	var template string
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.Name = args[0]
			if template != "" {
				if err := json.Unmarshal([]byte(template), &p.Template); err != nil {
					return fmt.Errorf("--template: %w", err)
				}
			}
			return a.c.Do(ctxOf(cmd), "POST", "/v1/pools", p, nil)
		},
	}
	var forceEvict bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a pool (its provisioned hosts are cordoned and terminated once idle)",
		Long: `Remove a pool. Its provisioned hosts are cordoned (no new placements) and
terminated once idle; their live Runs finish where they are.
--force-evict stops those Runs too, so they resume elsewhere.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/v1/pools/" + args[0]
			if forceEvict {
				q := url.Values{}
				q.Set("forceEvict", "true")
				path += "?" + q.Encode()
			}
			return a.c.Do(ctxOf(cmd), "DELETE", path, nil, nil)
		},
	}
	rm.Flags().BoolVar(&forceEvict, "force-evict", false, "also stop the pool's live Runs so they resume elsewhere")
	defer cmd.AddCommand(rm)
	set.Flags().StringVar(&p.Provider, "provider", "static", "static | ec2")
	set.Flags().IntVar(&p.MinHosts, "min", 0, "minimum hosts")
	set.Flags().IntVar(&p.MaxHosts, "max", 0, "maximum hosts")
	set.Flags().IntVar(&p.WarmHosts, "warm", 0, "idle hosts to keep ready")
	set.Flags().DurationVar(&p.ScaleDownAfter.Duration, "scale-down-after", 0, "how long a host stays idle before it is released (default: luxd's scale_down_after, 10m)")
	set.Flags().BoolVar(&p.WarmWhileActive, "warm-while-active", false, "keep --warm hosts only while the pool is in use; an idle pool scales down to --min")
	set.Flags().StringVar(&template, "template", "", "provider template (JSON)")
	cmd.AddCommand(ls, set)
	return cmd
}

// umask is the process's file-creation mask (read by setting it back).
func umask() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}
