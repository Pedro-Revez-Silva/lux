package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
)

func TestMoney(t *testing.T) {
	for _, c := range []struct{ amount, currency, want string }{
		{"1.28431", "USD", "1.2843 USD"},
		{"0.150000001", "USD", "0.15 USD"},
		{"2", "EUR", "2 EUR"},
		{"10.5", "USD", "10.5 USD"},
		{"0", "USD", "0 USD"},
		{"0.00005", "USD", "<0.0001 USD"}, // half-even: 0.0000|5 rounds to 0, but it is not nothing
		{"0.000074", "USD", "0.0001 USD"},
		{"0.00004", "USD", "<0.0001 USD"},
		{"0.00015", "USD", "0.0002 USD"}, // half to even: up
		{"0.00025", "USD", "0.0002 USD"}, // half to even: down
		{"0.000250001", "USD", "0.0003 USD"},
		{"1.99995", "USD", "2 USD"},
		{"999999999999999.999999999", "USD", "1000000000000000 USD"},
		{"-0.5", "EUR", "-0.5 EUR"},
		{"-0.00015", "EUR", "-0.0002 EUR"},
		{"-0.00001", "EUR", ">-0.0001 EUR"},
		{"123456789012345.12345", "JPY", "123456789012345.1234 JPY"},
		{"", "USD", "—"},
		{"1.5", "", "1.5"},
		{"abc", "USD", "abc USD"},
	} {
		if got := money(c.amount, c.currency); got != c.want {
			t.Errorf("money(%q, %q) = %q, want %q", c.amount, c.currency, got, c.want)
		}
	}
}

func TestRunCostCell(t *testing.T) {
	total := func(cur, amount, final, estimate string) server.CostTotal {
		return server.CostTotal{Currency: cur, Amount: amount, Final: final, Estimate: estimate}
	}
	for _, c := range []struct {
		cost *server.RunCostBrief
		want string
	}{
		{nil, "—"},
		{&server.RunCostBrief{Status: "pending", Totals: []server.CostTotal{}}, "—"},
		{&server.RunCostBrief{Status: "final", Totals: []server.CostTotal{total("USD", "0.150000001", "0.150000001", "0")}}, "0.15 USD"},
		{&server.RunCostBrief{Status: "complete", Totals: []server.CostTotal{total("USD", "0.2", "0.15", "0.05")}}, "~0.2 USD"},
		{&server.RunCostBrief{Status: "incomplete", Totals: []server.CostTotal{total("USD", "3", "3", "0")}}, "~3 USD"},
		{&server.RunCostBrief{Status: "complete", Totals: []server.CostTotal{total("EUR", "1", "1", "0"), total("USD", "2", "2", "0")}}, "multi"},
	} {
		if got := runCostCell(c.cost); got != c.want {
			t.Errorf("runCostCell(%+v) = %q, want %q", c.cost, got, c.want)
		}
	}
}

// fakeLuxd answers each path with a JSON body (the server's own types) or,
// for an int status, with luxd's error envelope. It records the queries.
type fakeLuxd struct {
	bodies map[string]any
	errors map[string][2]string // path → status text code, message
	status map[string]int
	seen   []string
}

func (f *fakeLuxd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.seen = append(f.seen, r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	if st, ok := f.status[r.URL.Path]; ok {
		w.WriteHeader(st)
		e := f.errors[r.URL.Path]
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": e[0], "message": e[1]}})
		return
	}
	b, ok := f.bodies[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		return
	}
	_ = json.NewEncoder(w).Encode(b)
}

// runCLI runs lux with args against f, returning stdout and the error.
func runCLI(t *testing.T, f http.Handler, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	root := a.root()
	root.SetArgs(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
	err := root.Execute()
	return out.String(), err
}

var costT0 = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func fixtureRunCost() server.RunCost {
	answered := costT0.Add(90 * time.Minute)
	next := costT0.Add(2 * time.Hour)
	return server.RunCost{
		RunID: "run_1", Status: "incomplete", Basis: "list",
		Totals: []server.CostTotal{
			{Currency: "EUR", Amount: "2.5", Final: "0", Estimate: "2.5"},
			{Currency: "USD", Amount: "1.434310001", Final: "0.150000001", Estimate: "1.28431"},
		},
		ByFamily: []server.CostTotal{
			{Family: "ai", DisplayName: "AI models", Color: "violet", Currency: "USD", Amount: "1.28431", Final: "0", Estimate: "1.28431"},
			{Family: "compute", DisplayName: "Compute", Currency: "USD", Amount: "0.150000001", Final: "0.150000001", Estimate: "0"},
			{Family: "video", Currency: "EUR", Amount: "2.5", Final: "0", Estimate: "2.5"},
		},
		Lines: []server.CostLine{
			{RunID: "run_1", Source: "model-gateway", Family: "ai", Item: "large-model-v3", Amount: "1.28431", Currency: "USD",
				From: costT0, To: costT0.Add(41 * time.Minute), Details: map[string]any{"requests": 57}, Reported: answered},
			{RunID: "run_1", Source: "compute", Family: "compute", Item: "m7i.2xlarge", Amount: "0.150000001", Currency: "USD",
				From: costT0, To: costT0.Add(time.Hour), Final: true, Details: map[string]any{}, Reported: answered},
			{RunID: "run_1", Source: "video-gw", Family: "video", Item: "", Amount: "2.5", Currency: "EUR",
				From: costT0, To: costT0.Add(time.Hour), Details: map[string]any{}, Reported: answered},
		},
		Sources: []server.CostSource{
			{Source: "compute", Status: "final", AnsweredAt: &answered},
			{Source: "model-gateway", Status: "ok", AnsweredAt: &answered, NextAt: &next},
			{Source: "video-gw", Status: "incomplete"},
		},
	}
}

func lineWith(t *testing.T, out string, fields ...string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		i := 0
		for _, w := range f {
			if i < len(fields) && w == fields[i] {
				i++
			}
		}
		if i == len(fields) {
			return
		}
	}
	t.Errorf("no line with %q in order in:\n%s", fields, out)
}

func TestCostCommand(t *testing.T) {
	f := &fakeLuxd{bodies: map[string]any{"/v1/runs/run_1/cost": fixtureRunCost()}}
	out, err := runCLI(t, f, "cost", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "list price"); n != 1 {
		t.Errorf("\"list price\" said %d times:\n%s", n, out)
	}
	lineWith(t, out, "status:", "incomplete", "(not", "answered:", "video-gw)")
	// Totals per currency with their split; never one sum across currencies.
	lineWith(t, out, "TOTAL", "FINAL", "ESTIMATE")
	lineWith(t, out, "2.5", "EUR", "0", "EUR", "2.5", "EUR")
	lineWith(t, out, "1.4343", "USD", "0.15", "USD", "1.2843", "USD")
	if strings.Contains(out, "3.9343") {
		t.Errorf("USD and EUR were added:\n%s", out)
	}
	lineWith(t, out, "AI", "models", "(ai)", "1.2843", "USD")
	lineWith(t, out, "Compute", "(compute)", "0.15", "USD")
	lineWith(t, out, "video", "2.5", "EUR")
	// Lines by family, with item, amount, currency, window and kind.
	lineWith(t, out, "AI", "models", "(ai)", "large-model-v3", "model-gateway", "1.2843", "USD", "estimate")
	lineWith(t, out, "m7i.2xlarge", "compute", "0.15", "USD", "final")
	lineWith(t, out, "video", "—", "video-gw", "2.5", "EUR", "estimate")
	// Sources: status and answeredAt; one that never answered shows —.
	lineWith(t, out, "compute", "final", costT0.Add(90*time.Minute).Local().Format("15:04:05"))
	lineWith(t, out, "video-gw", "incomplete", "—")

	out, err = runCLI(t, f, "cost", "run_1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatal(err)
	}
	// Raw: the exact amount, not the display rounding.
	if tot := back["totals"].([]any)[1].(map[string]any); tot["amount"] != "1.434310001" {
		t.Errorf("json totals: %v", back["totals"])
	}
}

func TestCostCommandPending(t *testing.T) {
	f := &fakeLuxd{bodies: map[string]any{"/v1/runs/run_2/cost": server.RunCost{
		RunID: "run_2", Status: "pending", Basis: "list", Totals: []server.CostTotal{}, ByFamily: []server.CostTotal{},
		Lines: []server.CostLine{}, Sources: []server.CostSource{},
	}}}
	out, err := runCLI(t, f, "cost", "run_2")
	if err != nil {
		t.Fatal(err)
	}
	lineWith(t, out, "status:", "pending")
	lineWith(t, out, "total:", "—")
	if strings.Contains(out, " 0 ") || strings.Contains(out, "0 USD") {
		t.Errorf("pending shown as zero:\n%s", out)
	}
	if _, err := runCLI(t, f, "cost", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown run: %v", err)
	}
}

func fixtureSummary() server.CostSummaryBody {
	day2 := costT0.Add(24 * time.Hour)
	return server.CostSummaryBody{
		From: costT0, To: costT0.Add(48 * time.Hour), Basis: "list",
		Totals: []server.CostSummaryRow{
			{Group: map[string]string{"family": "ai"}, Currency: "USD", Amount: "1.28431"},
			{Group: map[string]string{"family": "compute"}, Currency: "EUR", Amount: "0.5"},
			{Group: map[string]string{"family": "compute"}, Currency: "USD", Amount: "0.150000001"},
		},
		Series: []server.CostSummaryRow{
			{At: &costT0, Group: map[string]string{"family": "compute"}, Currency: "USD", Amount: "0.1"},
			{At: &day2, Group: map[string]string{"family": "compute"}, Currency: "USD", Amount: "0.050000001"},
		},
		Unallocated: []server.CostSummaryRow{{Currency: "USD", Amount: "0.75"}},
		Hosts:       []server.HostAllocation{{HostID: "host_a", Currency: "USD", Allocated: "1.23456", Unallocated: "0.25"}},
	}
}

func TestCostsCommand(t *testing.T) {
	f := &fakeLuxd{bodies: map[string]any{"/v1/costs": fixtureSummary()}}
	out, err := runCLI(t, f, "costs", "--by", "family", "--interval", "day")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.seen[len(f.seen)-1]; got != "/v1/costs?group=family&interval=day&since=7d" {
		t.Errorf("query %s", got)
	}
	if n := strings.Count(out, "list price"); n != 1 {
		t.Errorf("\"list price\" said %d times:\n%s", n, out)
	}
	lineWith(t, out, "FAMILY", "TOTAL")
	lineWith(t, out, "ai", "1.2843", "USD")
	lineWith(t, out, "compute", "0.5", "EUR")
	lineWith(t, out, "compute", "0.15", "USD")
	lineWith(t, out, "DAY", "FAMILY", "AMOUNT")
	lineWith(t, out, "Sep", "26", "compute", "0.1", "USD")
	lineWith(t, out, "Sep", "27", "compute", "0.05", "USD")
	lineWith(t, out, "unallocated", "0.75", "USD")

	out, err = runCLI(t, f, "costs", "--by", "host")
	if err != nil {
		t.Fatal(err)
	}
	lineWith(t, out, "HOST", "ALLOCATED", "UNALLOCATED")
	h := fixtureSummary().Hosts[0]
	row := append([]string{h.HostID}, strings.Fields(money(h.Allocated, h.Currency))...)
	lineWith(t, out, append(row, strings.Fields(money(h.Unallocated, h.Currency))...)...)

	_, err = runCLI(t, f, "costs", "--from", "2026-09-01T00:00:00Z", "--to", "2026-09-02T00:00:00Z",
		"--by", "tenant", "--by", "label:team", "--family", "ai", "--tenant", "acme")
	if err != nil {
		t.Fatal(err)
	}
	want := "/v1/costs?family=ai&from=2026-09-01T00%3A00%3A00Z&group=tenant&group=label%3Ateam&to=2026-09-02T00%3A00%3A00Z&tenant=acme"
	if got := f.seen[len(f.seen)-1]; got != want {
		t.Errorf("query\n got %s\nwant %s", got, want)
	}

	out, err = runCLI(t, f, "costs", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var back server.CostSummaryBody
	if err := json.Unmarshal([]byte(out), &back); err != nil || back.Totals[2].Amount != "0.150000001" {
		t.Errorf("json: %v %+v", err, back)
	}
}

func TestCostsSinceAndFrom(t *testing.T) {
	f := &fakeLuxd{bodies: map[string]any{"/v1/costs": fixtureSummary()}}
	if _, err := runCLI(t, f, "costs", "--since", "1d", "--from", "2026-09-01T00:00:00Z"); err == nil {
		t.Error("--since with --from was accepted")
	}
	if len(f.seen) != 0 {
		t.Errorf("requests sent: %v", f.seen)
	}
}

// The server's 400 and 413 messages reach the user as they are.
func TestCostsCommandErrors(t *testing.T) {
	for status, msg := range map[int]string{
		http.StatusBadRequest:            "cost range must not exceed 90 days",
		http.StatusRequestEntityTooLarge: "cost response exceeds 10000 rows",
	} {
		f := &fakeLuxd{status: map[string]int{"/v1/costs": status}, errors: map[string][2]string{"/v1/costs": {"bad_request", msg}}}
		_, err := runCLI(t, f, "costs", "--since", "100d")
		if err == nil || err.Error() != msg {
			t.Errorf("%d: got %v, want %q", status, err, msg)
		}
	}
}

func TestLsCostColumn(t *testing.T) {
	run := func(id string, cost *server.RunCostBrief) server.Run {
		return server.Run{ID: id, Tenant: "t", State: "running", Labels: map[string]string{}, CreatedAt: time.Now(), Cost: cost}
	}
	f := &fakeLuxd{bodies: map[string]any{"/v1/runs": map[string]any{"runs": []server.Run{
		run("run_one", &server.RunCostBrief{Status: "final", Totals: []server.CostTotal{{Currency: "USD", Amount: "0.150000001", Final: "0.150000001", Estimate: "0"}}}),
		run("run_est", &server.RunCostBrief{Status: "complete", Totals: []server.CostTotal{{Currency: "USD", Amount: "1.28431", Final: "0", Estimate: "1.28431"}}}),
		run("run_multi", &server.RunCostBrief{Status: "complete", Totals: []server.CostTotal{
			{Currency: "EUR", Amount: "1", Final: "1", Estimate: "0"}, {Currency: "USD", Amount: "2", Final: "2", Estimate: "0"}}}),
		run("run_pending", &server.RunCostBrief{Status: "pending", Totals: []server.CostTotal{}}),
	}}}}
	out, err := runCLI(t, f, "ls")
	if err != nil {
		t.Fatal(err)
	}
	lineWith(t, out, "ID", "NAME", "STATE", "HOST", "ADAPTER", "RUNTIME", "COST", "CREATED")
	lineWith(t, out, "run_one", "running", "0.15", "USD")
	lineWith(t, out, "run_est", "running", "~1.2843", "USD")
	lineWith(t, out, "run_multi", "running", "multi")
	lineWith(t, out, "run_pending", "running", "—")
}

func TestLsRuntimeColumn(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	run := func(id string, secs float64, since *time.Time) server.Run {
		return server.Run{ID: id, Tenant: "t", State: "running", Labels: map[string]string{}, CreatedAt: time.Now(), RuntimeSeconds: secs, RuntimeSince: since}
	}
	f := &fakeLuxd{bodies: map[string]any{"/v1/runs": map[string]any{"runs": []server.Run{
		run("run_never", 0, nil),
		run("run_short", 45.9, nil),
		run("run_min", 200, nil),
		run("run_hour", 2*3600+5*60+59, nil),
		run("run_day", 86400+3*3600, nil),
		run("run_even", 3600, nil),
		run("run_live", 0.4, &since),
	}}}}
	out, err := runCLI(t, f, "ls")
	if err != nil {
		t.Fatal(err)
	}
	lineWith(t, out, "ID", "NAME", "STATE", "HOST", "ADAPTER", "RUNTIME", "COST", "CREATED")
	lineWith(t, out, "run_never", "running", "-", "-", "—")
	lineWith(t, out, "run_short", "running", "45s", "—")
	lineWith(t, out, "run_min", "running", "3m20s", "—")
	lineWith(t, out, "run_hour", "running", "2h5m", "—")
	lineWith(t, out, "run_day", "running", "1d3h", "—")
	lineWith(t, out, "run_even", "running", "1h", "—")
	lineWith(t, out, "run_live", "running", "0s", "—")
}
