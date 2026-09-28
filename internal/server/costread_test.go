package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

func costReadFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool, state) VALUES
		('h1', 'h1', 't1', 'blue', 'ready'), ('h2', 'h2', 't2', 'red', 'ready'),
		('hp', 'hp', NULL, 'shared', 'ready')`)
	execSQL(t, s, ctx, `UPDATE runs SET labels = '{"team":"alpha"}' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool, amount, allocated, unallocated) VALUES
		($1, 't1', 'r1', 'compute', 'compute', 'USD', 'h1', 'blue', 1.250000000, 0, 0),
		($1, 't1', 'r1', 'plugin', 'ai', 'EUR', NULL, NULL, 2.500000000, 0, 0),
		($1, 't2', 'r2', 'compute', 'compute', 'USD', 'h2', 'red', 7, 0, 0),
		($2, 't1', 'r1', 'compute', 'compute', 'USD', 'h1', 'blue', 0.750000000, 0, 0),
		($2, NULL, NULL, 'compute', 'compute', 'USD', 'h1', 'blue', 0, 2, 0.250000000),
		($2, NULL, NULL, 'compute', 'compute', 'EUR', 'hp', 'shared', 0, 0.5, 1.5),
		($1, NULL, NULL, 'compute', 'compute', 'USD', 'h2', 'red', 0, 7, 3)`, t0, t0.Add(time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h1', $1, 2.250000000, 'USD', 4, 1024, 'static')`, t0)
	return s, keys
}

func costPath(q string) string {
	return "/v1/costs?from=" + url.QueryEscape(t0.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(t0.Add(2*time.Hour).Format(time.RFC3339)) + q
}

func TestCostSummaryScopeAndGroups(t *testing.T) {
	s, keys := costReadFixture(t)
	var got costSummaryBody
	if code := getJSON(t, s, keys["t1"], costPath("&group=pool&group=family&interval=hour"), &got); code != http.StatusOK {
		t.Fatalf("tenant status: %d", code)
	}
	byPool := map[string]string{}
	for _, row := range got.Totals {
		byPool[row.Group["pool"]] = row.Amount
	}
	if len(got.Totals) != 2 || byPool["(none)"] != "2.5" || byPool["blue"] != "2" || len(got.Series) != 3 || got.Unallocated != nil || got.Hosts != nil {
		t.Errorf("tenant summary: %+v", got)
	}
	if code := getJSON(t, s, keys["t1"], costPath("&group=tenant"), &got); code != http.StatusBadRequest {
		t.Errorf("tenant group status %d", code)
	}
	if code := getJSON(t, s, keys["op"], costPath("&group=tenant&group=label:team"), &got); code != http.StatusOK {
		t.Fatalf("operator status %d", code)
	}
	if len(got.Totals) != 3 || got.Totals[0].Group["tenant"] != "t1" || got.Totals[0].Group["label:team"] != "alpha" ||
		len(got.Unallocated) != 2 || got.Unallocated[0].Currency != "EUR" || got.Unallocated[0].Amount != "1.5" ||
		got.Unallocated[1].Amount != "3.25" {
		t.Errorf("operator summary: %+v", got)
	}
	got = costSummaryBody{}
	if code := getJSON(t, s, keys["op"], costPath("&tenant=t1&group=host"), &got); code != http.StatusOK || len(got.Totals) != 2 || got.Unallocated != nil || got.Hosts != nil {
		t.Errorf("narrowed summary: %d %+v", code, got)
	}
	got = costSummaryBody{}
	if code := getJSON(t, s, keys["op"], costPath("&group=host&family=compute"), &got); code != http.StatusOK || len(got.Hosts) != 3 || got.Hosts[0].Allocated != "2" {
		t.Errorf("host breakdown: %d %+v", code, got)
	}
}

func TestCostSummaryRangeAndValidation(t *testing.T) {
	s, keys := costReadFixture(t)
	var got costSummaryBody
	path := "/v1/costs?from=" + url.QueryEscape(t0.Add(time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(2*time.Hour).Format(time.RFC3339))
	if code := getJSON(t, s, keys["t1"], path+"&interval=day", &got); code != http.StatusOK || len(got.Totals) != 1 || got.Totals[0].Amount != "0.75" || len(got.Series) != 1 || got.Series[0].At == nil || !got.Series[0].At.Equal(t0.Truncate(24*time.Hour)) {
		t.Errorf("range: %d %+v", code, got)
	}
	for _, q := range []string{"&group=host&group=pool&group=run", "&group=label:", "&group=host&group=host", "&interval=week"} {
		if code := getJSON(t, s, keys["op"], costPath(q), &got); code != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, code)
		}
	}
	for _, path := range []string{"/v1/costs?from=broken", "/v1/costs?from=2026-09-26T11:00:00Z&to=2026-09-26T09:00:00Z"} {
		if code := getJSON(t, s, keys["op"], path, &got); code != http.StatusBadRequest {
			t.Errorf("%s: status %d", path, code)
		}
	}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:team%27%20OR%201%3D1--"), &got); code != http.StatusOK || len(got.Totals) != 2 || got.Totals[0].Group["label:team' OR 1=1--"] != "(none)" {
		t.Errorf("label key: %d %+v", code, got)
	}
}

func TestCostReadEffectiveBounds(t *testing.T) {
	s, keys := costReadFixture(t)
	for _, tc := range []struct {
		query    string
		from, to time.Time
		amount   string
	}{
		{"from=" + url.QueryEscape(t0.Add(30*time.Minute).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(75*time.Minute).Format(time.RFC3339)), t0, t0.Add(2 * time.Hour), "2"},
		{"from=" + url.QueryEscape(t0.Add(30*time.Minute).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(time.Hour).Format(time.RFC3339)), t0, t0.Add(time.Hour), "1.25"},
	} {
		var summary costSummaryBody
		if code := getJSON(t, s, keys["t1"], "/v1/costs?"+tc.query+"&family=compute", &summary); code != 200 || !summary.From.Equal(tc.from) || !summary.To.Equal(tc.to) || len(summary.Totals) != 1 || summary.Totals[0].Amount != tc.amount {
			t.Errorf("summary %s: %d %+v", tc.query, code, summary)
		}
		var host struct {
			From  time.Time      `json:"from"`
			To    time.Time      `json:"to"`
			Hours []hostCostHour `json:"hours"`
		}
		wantHours := 0
		if tc.to.After(t0.Add(time.Hour)) {
			wantHours = 1
		}
		if code := getJSON(t, s, keys["op"], "/v1/hosts/h1/cost?"+tc.query, &host); code != 200 || !host.From.Equal(tc.from) || !host.To.Equal(tc.to) || len(host.Hours) != wantHours {
			t.Errorf("host %s: %d %+v", tc.query, code, host)
		}
	}
	for _, path := range []string{"/v1/costs", "/v1/hosts/h1/cost"} {
		var body struct{ From, To time.Time }
		if code := getJSON(t, s, keys["op"], path, &body); code != 200 || body.From.Nanosecond() != 0 || body.From.Minute() != 0 || body.To.Minute() != 0 || body.To.Sub(body.From) != 2*time.Hour {
			t.Errorf("default %s: %d %+v", path, code, body)
		}
		q := "?from=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(costMaxRange+time.Second).Format(time.RFC3339))
		if code := getJSON(t, s, keys["op"], path+q, &body); code != http.StatusBadRequest {
			t.Errorf("oversize %s: %d", path, code)
		}
	}
}

func TestCostSummaryConcurrentReplacement(t *testing.T) {
	s, keys := costReadFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Replace run and host hours together, as a single producer commit.
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE cost_hourly SET amount = CASE WHEN amount = 1.25 THEN 4 ELSE 1.25 END WHERE run_id = 'r1' AND source = 'compute' AND hour = $1`, t0)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE cost_hourly SET unallocated = CASE WHEN unallocated = 0.25 THEN 8 ELSE 0.25 END, allocated = CASE WHEN allocated = 2 THEN 9 ELSE 2 END WHERE run_id IS NULL AND host_id = 'h1' AND hour = $1`, t0.Add(time.Hour))
				return err
			})
			if err != nil {
				t.Errorf("replacement: %v", err)
				return
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for range 40 {
		var got costSummaryBody
		if code := getJSON(t, s, keys["op"], costPath("&group=host&family=compute&interval=hour"), &got); code != 200 {
			t.Fatalf("summary status: %d", code)
		}
		if len(got.Totals) != 2 || len(got.Series) != 3 || len(got.Unallocated) != 2 || len(got.Hosts) != 3 {
			t.Fatalf("summary shape: %+v", got)
		}
		var amount, series string
		for _, row := range got.Totals {
			if row.Group["host"] == "h1" {
				amount = row.Amount
			}
		}
		for _, row := range got.Series {
			if row.Group["host"] == "h1" && row.At.Equal(t0) {
				series = row.Amount
			}
		}
		var host hostAllocation
		for _, row := range got.Hosts {
			if row.HostID == "h1" {
				host = row
			}
		}
		var unallocated string
		for _, row := range got.Unallocated {
			if row.Currency == "USD" {
				unallocated = row.Amount
			}
		}
		if !((amount == "2" && series == "1.25" && unallocated == "3.25" && host.Allocated == "2" && host.Unallocated == "0.25") ||
			(amount == "4.75" && series == "4" && unallocated == "11" && host.Allocated == "9" && host.Unallocated == "8")) {
			t.Errorf("mixed snapshot: %+v", got)
		}
	}
}

func TestHostCostRowLimit(t *testing.T) {
	s, keys := costReadFixture(t)
	execSQL(t, s, context.Background(), `INSERT INTO cost_hourly (hour, host_id, source, family, currency)
		SELECT $1, 'h1', 'compute', 'compute', 'C' || n::text FROM generate_series(1, 10001) AS n`, t0)
	var got struct {
		Hours []hostCostHour `json:"hours"`
	}
	if code := getJSON(t, s, keys["op"], "/v1/hosts/h1/cost?from="+url.QueryEscape(t0.Format(time.RFC3339))+"&to="+url.QueryEscape(t0.Add(time.Hour).Format(time.RFC3339)), &got); code != http.StatusRequestEntityTooLarge {
		t.Errorf("row limit status: %d", code)
	}
}

func TestHostCostVisibility(t *testing.T) {
	s, keys := costReadFixture(t)
	type hostResponse struct {
		Hours []hostCostHour `json:"hours"`
		Rates []hostCostRate `json:"rates"`
	}
	var got hostResponse
	for _, tc := range []struct {
		key, host string
		code      int
	}{
		{keys["t1"], "h1", 200}, {keys["t1"], "h2", 404}, {keys["t1"], "hp", 403},
		{keys["op"], "hp", 200}, {keys["op"], "missing", 404},
	} {
		got = hostResponse{}
		path := fmt.Sprintf("/v1/hosts/%s/cost?from=%s&to=%s", tc.host, url.QueryEscape(t0.Format(time.RFC3339)), url.QueryEscape(t0.Add(2*time.Hour).Format(time.RFC3339)))
		code := getJSON(t, s, tc.key, path, &got)
		if code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.host, code, tc.code)
		}
		if code == 200 {
			if len(got.Hours) != 1 {
				t.Errorf("%s hours: %+v", tc.host, got.Hours)
			}
			if tc.host == "h1" && (got.Hours[0].Allocated != "2" || got.Hours[0].Unallocated != nil || len(got.Rates) != 0) {
				t.Errorf("tenant host cost: %+v", got)
			}
			if tc.host == "hp" && (got.Hours[0].Unallocated == nil || *got.Hours[0].Unallocated != "1.5") {
				t.Errorf("platform host: %+v", got)
			}
		}
	}
	path := fmt.Sprintf("/v1/hosts/h1/cost?from=%s&to=%s", url.QueryEscape(t0.Format(time.RFC3339)), url.QueryEscape(t0.Add(2*time.Hour).Format(time.RFC3339)))
	if code := getJSON(t, s, keys["op"], path, &got); code != 200 || len(got.Rates) != 1 || got.Rates[0].PerHour != "2.25" || got.Hours[0].Unallocated == nil {
		t.Errorf("operator host cost: %d %+v", code, got)
	}
	if code := getJSON(t, s, keys["op"], path+"&tenant=t2", &got); code != 404 {
		t.Errorf("narrowed host: %d", code)
	}
}
