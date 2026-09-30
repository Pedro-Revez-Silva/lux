package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
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
		// Rows written before capacity planning carry no plan counts.
		if _, planned := d["ready"]; planned {
			line += capacityPlanText(d)
		}
	case "pool.scale_blocked":
		line = fmt.Sprintf("no host launched: %s waiting, had %s, max %s", s("waiting"), s("total"), s("max")) + capacityPlanText(d)
	case "host.capacity_decision":
		line = fmt.Sprintf("%s (%s) in pool %s", s("decision"), strings.ReplaceAll(s("stage"), "_", " "), s("pool"))
		if b := blockersText(d["blockers"]); b != "" {
			line += ": " + b
		} else if r := s("reason"); r != "" {
			line += ": " + r
		}
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
		if r := resourcesText(d["resources"]); r != "" {
			line += " (" + r + ")"
		}
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
		line = s("from") + " → " + s("to")
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

// capacityPlanText is a scale-up's capacity plan: Run counts, the expected
// new-host capacity, and sampled blocker evidence.
func capacityPlanText(d map[string]any) string {
	num := func(k string) string {
		if v, ok := d[k].(float64); ok {
			return plainNumber(v)
		}
		return "0"
	}
	parts := []string{fmt.Sprintf("plan: %s ready, %s starting, %s planned, %s unmet, %s blocked",
		num("ready"), num("starting"), num("planned"), num("unmet"), num("blocked"))}
	if d["probe"] == true {
		parts = append(parts, "probe: one host to re-observe capacity no expected host fits")
	}
	if exp, ok := d["expected"].(map[string]any); ok {
		c, _ := exp["capacity"].(map[string]any)
		parts = append(parts, fmt.Sprintf("new host %s, %s, %s, %s from %s observation(s)",
			capacityText(c, "cpus"), capacityText(c, "memory"), capacityText(c, "disk"), capacityText(c, "runs"), plainNumber(exp["observations"])))
	} else if u, _ := d["unknown"].(string); u != "" {
		parts = append(parts, "new host capacity unknown: "+u)
	}
	if v := deficitsText(d["deficits"], false); v != "" {
		parts = append(parts, "deficits: "+v)
	}
	if v := deficitsText(d["exhausted"], true); v != "" {
		parts = append(parts, "exhausted: "+v)
	}
	if list, _ := d["ineligible"].([]any); len(list) > 0 {
		var hosts []string
		for _, x := range list {
			h, _ := x.(map[string]any)
			hosts = append(hosts, fmt.Sprintf("%v %v", h["host"], h["reason"]))
		}
		parts = append(parts, "ineligible: "+strings.Join(hosts, ", "))
	}
	if n, _ := d["omitted"].(float64); n > 0 {
		parts = append(parts, plainNumber(n)+" more omitted")
	}
	return "; " + strings.Join(parts, "; ")
}

// capacityText is one expected-capacity resource; zero is an observed unlimited.
func capacityText(c map[string]any, resource string) string {
	v, ok := c[resource].(float64)
	if !ok || v == 0 {
		return resource + " unlimited"
	}
	return resource + " " + resourceAmount(resource, v)
}

// deficitsText is sampled evidence: "r1 new host [memory requested 8.0 GiB, ...]".
// onHost names the actual host whose fit failed.
func deficitsText(v any, onHost bool) string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		e, _ := x.(map[string]any)
		stage := strings.ReplaceAll(fmt.Sprint(e["stage"]), "_", " ")
		head := fmt.Sprintf("%v %s", e["run"], stage)
		if onHost {
			head = fmt.Sprintf("%v (%s) for %v", e["host"], stage, e["run"])
		}
		out = append(out, head+" ["+blockersText(e["blockers"])+"]")
	}
	return strings.Join(out, ", ")
}

// blockersText renders planner blockers: resource numbers in their units, or a constraint reason.
func blockersText(v any) string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		b, _ := x.(map[string]any)
		res, _ := b["resource"].(string)
		if res == "" {
			out = append(out, fmt.Sprint(b["reason"]))
			continue
		}
		num := func(k string) string {
			if n, ok := b[k].(float64); ok {
				return resourceAmount(res, n)
			}
			return "?"
		}
		out = append(out, fmt.Sprintf("%s requested %s, used %s, capacity %s, available %s",
			res, num("requested"), num("used"), num("capacity"), num("available")))
	}
	return strings.Join(out, "; ")
}

// resourcesText is a placement's requested resources, if its event has them.
func resourcesText(v any) string {
	r, _ := v.(map[string]any)
	var out []string
	for _, k := range []string{"cpus", "memory", "disk", "pids"} {
		if n, ok := r[k].(float64); ok && n != 0 {
			out = append(out, k+" "+resourceAmount(k, n))
		}
	}
	return strings.Join(out, ", ")
}

// resourceAmount formats memory and disk as bytes, other resources as counts.
func resourceAmount(resource string, v float64) string {
	if resource != "memory" && resource != "disk" {
		return plainNumber(v)
	}
	if v < 0 {
		return "-" + bytesHuman(int64(-v))
	}
	return bytesHuman(int64(v))
}

// plainNumber prints a JSON number without exponent notation.
func plainNumber(v any) string {
	n, ok := v.(float64)
	if !ok {
		return "?"
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}
