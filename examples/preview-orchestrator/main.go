// Command preview-orchestrator is a reference owner of a wakeable lux
// server: the smallest program that turns a lux server into a branch
// preview that sleeps when nobody looks and wakes on the latest commit.
//
// It creates (or finds) one server at -hostname that wakes on request, then
// follows the tenant's event feed (GET /v1/events, resuming with
// Last-Event-ID after a disconnect) and answers:
//
//   - server.wake_requested: the server's Run is resumed with a sync of the
//     branch (so it serves the branch's latest commit), or, with no Run to
//     resume, a servers-only Run is submitted and the server attached to it;
//   - server.idle: the Run is stopped (its state volume is snapshotted, so
//     the next wake carries on where it left off).
//
// lux itself never starts or stops a Run for a server; this program does.
// Only the Go standard library; run with `go run ./examples/preview-orchestrator -h`.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type config struct {
	luxURL, key                    string
	hostname, name, image          string
	repoURL, branch, repoName      string
	gitToken                       string
	command                        []string
	port                           int
	idleAfter, wakeTimeout, labels string
}

func main() {
	var c config
	var command, label string
	flag.StringVar(&c.luxURL, "lux", os.Getenv("LUX_URL"), "luxd's URL (LUX_URL)")
	flag.StringVar(&c.hostname, "hostname", "", "the preview's host name, under luxd's preview domain (e.g. web.pr9.lux.localhost)")
	flag.StringVar(&c.name, "name", "web", "the server's name")
	flag.IntVar(&c.port, "port", 8080, "the port the server listens on in the container")
	flag.StringVar(&command, "command", "", "the server's command (split on spaces)")
	flag.StringVar(&c.image, "image", "", "the image of the Runs it submits")
	flag.StringVar(&c.repoURL, "repo", "", "the git repository the preview serves")
	flag.StringVar(&c.repoName, "repo-name", "app", "its name in the Run's spec (checked out at /workspace/<name>)")
	flag.StringVar(&c.branch, "branch", "main", "the branch it previews")
	flag.StringVar(&c.idleAfter, "idle-after", "10m", "stop the Run after this long without a request")
	flag.StringVar(&c.wakeTimeout, "wake-timeout", "5m", "how long a waking page waits for the Run")
	flag.StringVar(&label, "label", "", "k=v label for the server (e.g. pr=9)")
	flag.Parse()
	c.key = os.Getenv("LUX_API_KEY")
	c.gitToken = os.Getenv("GIT_TOKEN")
	c.command = strings.Fields(command)
	c.labels = label
	if c.luxURL == "" || c.key == "" || c.hostname == "" || c.image == "" || c.repoURL == "" || len(c.command) == 0 {
		fmt.Fprintln(os.Stderr, "need LUX_URL, LUX_API_KEY, -hostname, -image, -repo and -command (GIT_TOKEN for a private repository)")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := &orchestrator{c: c, http: &http.Client{Timeout: 30 * time.Second}}
	if err := o.run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

type orchestrator struct {
	c        config
	http     *http.Client
	serverID string
}

// server is what this program reads of a lux server.
type server struct {
	ID       string  `json:"id"`
	URL      *string `json:"url"`
	State    string  `json:"state"`
	RunID    *string `json:"runId"`
	Hostname string  `json:"hostname"`
}

// event is a feed event (GET /v1/events).
type event struct {
	ID       int64          `json:"id"`
	Type     string         `json:"type"`
	ServerID string         `json:"serverId"`
	RunID    *string        `json:"runId"`
	Data     map[string]any `json:"data"`
}

func (o *orchestrator) run(ctx context.Context) error {
	sv, err := o.ensureServer(ctx)
	if err != nil {
		return err
	}
	o.serverID = sv.ID
	log.Printf("server %s at %s (%s)", sv.ID, deref(sv.URL), sv.State)
	// Follow the feed from now; after a disconnect, from the last event seen.
	last := ""
	for ctx.Err() == nil {
		err := o.follow(ctx, &last)
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("feed: %v; reconnecting", err)
		time.Sleep(2 * time.Second)
	}
	return nil
}

// ensureServer finds the server of -hostname, or creates it.
func (o *orchestrator) ensureServer(ctx context.Context) (server, error) {
	var list struct {
		Servers []server `json:"servers"`
	}
	if err := o.do(ctx, "GET", "/v1/servers?hostname="+url.QueryEscape(o.c.hostname), nil, &list); err != nil {
		return server{}, err
	}
	if len(list.Servers) > 0 {
		return list.Servers[0], nil
	}
	body := map[string]any{"name": o.c.name, "port": o.c.port, "command": o.c.command, "hostname": o.c.hostname,
		"wake": "request", "idleAfter": o.c.idleAfter, "wakeTimeout": o.c.wakeTimeout, "lifetime": "owner"}
	if k, v, ok := strings.Cut(o.c.labels, "="); ok {
		body["labels"] = map[string]string{k: v}
	}
	var sv server
	err := o.do(ctx, "POST", "/v1/servers", body, &sv)
	return sv, err
}

// follow reads the feed until it ends, handling this server's events.
func (o *orchestrator) follow(ctx context.Context, last *string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(o.c.luxURL, "/")+"/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.c.key)
	req.Header.Set("Accept", "text/event-stream")
	if *last != "" {
		req.Header.Set("Last-Event-ID", *last)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/events: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var id, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			id = line[4:]
		case strings.HasPrefix(line, "data: "):
			data += line[6:]
		case line == "":
			if data != "" {
				var e event
				if err := json.Unmarshal([]byte(data), &e); err == nil && e.ServerID == o.serverID {
					o.handle(ctx, e)
				}
				*last = id
			}
			id, data = "", ""
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the feed ended")
}

func (o *orchestrator) handle(ctx context.Context, e event) {
	switch e.Type {
	case "server.wake_requested":
		log.Printf("wake requested by %v for %v", e.Data["by"], e.Data["path"])
		// Off the feed's goroutine: waiting out a stop must not hold up
		// the events behind it.
		go func() {
			if err := o.wake(ctx, e.RunID); err != nil {
				log.Printf("wake: %v", err)
			}
		}()
	case "server.idle":
		if e.RunID == nil {
			return
		}
		log.Printf("idle (no request for %v): stopping %s", e.Data["idleAfter"], *e.RunID)
		if err := o.do(ctx, "POST", "/v1/runs/"+*e.RunID+"/stop", nil, nil); err != nil {
			log.Printf("stop: %v", err)
		}
	case "server.state":
		log.Printf("server %v", e.Data["state"])
	case "server.deleted", "server.expired":
		log.Printf("server %s is gone (%s); nothing more to do", o.serverID, e.Type)
	}
}

// wake brings a Run up for the server: its Run resumed with a sync of the
// branch, or a new servers-only Run.
func (o *orchestrator) wake(ctx context.Context, runID *string) error {
	secrets := []map[string]string{}
	if o.c.gitToken != "" {
		secrets = append(secrets, map[string]string{"name": "GIT_TOKEN", "value": o.c.gitToken})
	}
	if runID != nil {
		var run struct {
			State string `json:"state"`
		}
		if err := o.do(ctx, "GET", "/v1/runs/"+*runID, nil, &run); err != nil {
			return err
		}
		// A wake can come while the Run is still stopping (an idle stop
		// under way): resume it once it has stopped. lux does not ask
		// again while this wake is open.
		for deadline := time.Now().Add(3 * time.Minute); run.State == "stopping" && time.Now().Before(deadline); {
			time.Sleep(time.Second)
			if err := o.do(ctx, "GET", "/v1/runs/"+*runID, nil, &run); err != nil {
				return err
			}
		}
		switch run.State {
		case "stopped", "lost", "failed":
			log.Printf("resuming %s on %s's latest commit", *runID, o.c.branch)
			return o.do(ctx, "POST", "/v1/runs/"+*runID+"/resume", map[string]any{
				"secrets": secrets, "sync": []map[string]string{{"repo": o.c.repoName, "ref": o.c.branch}}}, nil)
		case "succeeded", "terminated":
			// Never runs again: a new one below.
		default:
			log.Printf("%s is %s already", *runID, run.State)
			return nil
		}
	}
	spec := o.spec(secrets)
	var run struct {
		ID string `json:"id"`
	}
	if err := o.do(ctx, "POST", "/v1/runs", spec, &run); err != nil {
		return err
	}
	if runID != nil {
		_ = o.do(ctx, "POST", "/v1/servers/"+o.serverID+"/detach", nil, nil)
	}
	log.Printf("submitted %s; attaching the server", run.ID)
	return o.do(ctx, "POST", "/v1/servers/"+o.serverID+"/attach", map[string]string{"runId": run.ID}, nil)
}

// spec is a servers-only Run: nothing but its servers runs (a generic
// workload that sleeps until stopped); the checkout and anything the server
// keeps live on a state volume, so they survive stops and moves.
func (o *orchestrator) spec(secrets []map[string]string) map[string]any {
	repo := map[string]any{"name": o.c.repoName, "url": o.c.repoURL, "ref": o.c.branch, "path": "/workspace/" + o.c.repoName}
	if o.c.gitToken != "" {
		repo["credential"] = "GIT_TOKEN"
	}
	return map[string]any{
		"name":     "preview " + o.c.hostname,
		"labels":   map[string]string{"preview": o.c.hostname},
		"image":    map[string]any{"ref": o.c.image},
		"workload": map[string]any{"adapter": "generic", "command": []string{"sleep", "infinity"}, "workdir": "/workspace"},
		"volumes":  []map[string]any{{"name": "workspace", "path": "/workspace", "kind": "state"}},
		"git":      map[string]any{"repositories": []any{repo}},
		"secrets":  secrets,
	}
}

func (o *orchestrator) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.c.luxURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, e.Error.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
