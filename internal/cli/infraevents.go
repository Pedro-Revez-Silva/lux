package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/server"
)

// infraEventsCmd is `lux pools events` and `lux hosts events`: newest
// first, a page at a time (--before an id, --limit), or every page with
// --all. query adds the command's own parameters.
func (a *app) infraEventsCmd(use, short, base string, query func(url.Values)) *cobra.Command {
	var before, limit int
	var all bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The server's bounds: out of them it would use its default,
			// and --all would page by the wrong size.
			if limit < 1 || limit > 1000 {
				return fmt.Errorf("--limit must be 1 to 1000")
			}
			var evs []server.LifecycleEvent
			for next := before; ; {
				q := url.Values{"limit": {fmt.Sprint(limit)}}
				if next > 0 {
					q.Set("before", fmt.Sprint(next))
				}
				if query != nil {
					query(q)
				}
				var resp struct {
					Events []server.LifecycleEvent `json:"events"`
				}
				if err := a.c.Do(ctxOf(cmd), "GET", base+url.PathEscape(args[0])+"/events?"+q.Encode(), nil, &resp); err != nil {
					return err
				}
				evs = append(evs, resp.Events...)
				if !all || len(resp.Events) < limit {
					break
				}
				next = int(resp.Events[len(resp.Events)-1].ID)
			}
			if a.output == "json" {
				return a.json(evs)
			}
			var rows [][]string
			for _, e := range evs {
				rows = append(rows, []string{e.Time.Local().Format(time.DateTime), e.Type, eventLine(e)})
			}
			a.table("TIME\tTYPE\tSUMMARY", rows)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "at most this many events (1 to 1000), newest first")
	cmd.Flags().IntVar(&before, "before", 0, "only events older than this event id (the next page)")
	cmd.Flags().BoolVar(&all, "all", false, "every event, page after page")
	return cmd
}

// poolEventsCmd is `lux pools events`; --platform picks the platform's
// pool where a tenant's has the same name.
func (a *app) poolEventsCmd() *cobra.Command {
	var platform bool
	cmd := a.infraEventsCmd("events <name>", "What happened to a pool: scale-ups, launches, placements, releases, newest first", "/v1/pools/",
		func(q url.Values) {
			if platform {
				q.Set("owner", "platform")
			}
		})
	cmd.Flags().BoolVar(&platform, "platform", false, "operators: the platform's pool of that name, not a tenant's")
	return cmd
}

// eventLine is one line saying what a pool or host event says.
func eventLine(e server.LifecycleEvent) string {
	d := e.Data
	s := func(k string) string {
		if v, ok := d[k]; ok && v != nil && v != "" {
			return fmt.Sprint(v)
		}
		return ""
	}
	var line string
	switch e.Type {
	case "pool.scale_up":
		line = fmt.Sprintf("+%s host(s) for %s: %s waiting, warm %s, min %s, max %s; had %s (%s idle, %s provisioning)",
			s("hosts"), s("reason"), s("waiting"), s("warm"), s("min"), s("max"), s("total"), s("idle"), s("provisioning"))
	case "pool.launch_requested":
		line = "launching " + s("name")
	case "pool.host_launched":
		line = strings.TrimSpace(fmt.Sprintf("%s is %s %s %s %s", s("name"), s("providerId"), s("instanceType"), s("market"), s("zone")))
	case "pool.launch_failed":
		line = "launch failed: " + s("error")
	case "pool.host_registered":
		line = s("name") + " registered"
	case "pool.host_released":
		line = s("name") + " released: " + s("reason")
		if v := s("idleSeconds"); v != "" {
			line += " for " + v + "s"
		}
	case "pool.spot_interrupted":
		line = s("host") + " interrupted: " + s("reason")
	case "pool.placement", "host.placement_assigned":
		line = fmt.Sprintf("%s epoch %s on %s", s("run"), s("epoch"), s("host"))
	case "pool.config_changed", "pool.retired", "pool.restored":
		changes, _ := d["changes"].(map[string]any)
		keys := make([]string, 0, len(changes))
		for k := range changes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			c, _ := changes[k].(map[string]any)
			parts = append(parts, fmt.Sprintf("%s %v→%v", k, orNone(c["old"]), orNone(c["new"])))
		}
		line = strings.Join(parts, ", ")
		if d["created"] == true {
			line = "created: " + line
		}
	case "pool.renamed":
		line = fmt.Sprintf("%s → %s: %s hosts, %s Runs, %s instances followed", s("from"), s("to"), s("hosts"), s("runs"), s("instances"))
		if d["isDefault"] == true {
			line += "; still the default"
		}
	case "pool.provider_error", "host.provider_error":
		line = s("op") + ": " + s("error")
	case "host.registered":
		line = strings.TrimSpace(s("name") + " " + s("arch") + " runner " + s("runner") + " " + s("providerId"))
	case "host.ready":
		line = "from " + s("from")
	case "host.placement_ended":
		line = fmt.Sprintf("%s epoch %s: %s", s("run"), s("epoch"), s("outcome"))
		if r := s("reason"); r != "" {
			line += " (" + r + ")"
		}
	case "host.drain_requested":
		line = s("cause") + ": " + s("reason")
		if d["evict"] == true {
			line += ", evicting its Runs"
		}
	default:
		line = s("reason")
	}
	if e.Count > 1 && e.LastTime != nil {
		line += fmt.Sprintf(" (×%d, last %s)", e.Count, e.LastTime.Local().Format(time.DateTime))
	}
	return line
}

func orNone(v any) any {
	if v == nil || v == "" {
		return "-"
	}
	return v
}
