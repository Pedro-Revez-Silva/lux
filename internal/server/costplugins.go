package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

const pluginBodyLimit = 16 << 20

type CostPluginConfig struct {
	Name, URL, TokenFile, TokenEnv string
	Timeout                        time.Duration
	MaxBatch                       int
	Settle                         []time.Duration
	Insecure                       bool
}

type pluginDescribe struct {
	Protocol []int  `json:"protocol"`
	Name     string `json:"name"`
	Families map[string]struct {
		DisplayName string `json:"displayName"`
		Color       string `json:"color"`
	} `json:"families"`
	MaxBatch int      `json:"maxBatch"`
	Settle   []string `json:"settle"`
}

type costPlugin struct {
	cfg       CostPluginConfig
	client    *http.Client
	mu        sync.RWMutex
	desc      pluginDescribe
	usable    bool
	failing   time.Time
	lastError string
}

type pluginRun struct {
	RunID      string            `json:"runId"`
	TenantID   string            `json:"tenantId"`
	TenantName string            `json:"tenantName"`
	Labels     map[string]string `json:"labels"`
	Adapter    string            `json:"adapter"`
	State      string            `json:"state"`
	Terminal   bool              `json:"terminal"`
	Window     struct {
		From time.Time  `json:"from"`
		To   *time.Time `json:"to"`
	} `json:"window"`
	Placements []struct {
		Epoch int        `json:"epoch"`
		From  time.Time  `json:"from"`
		To    *time.Time `json:"to"`
	} `json:"placements"`
	Sessions []struct {
		ID        string    `json:"id"`
		Epoch     int       `json:"epoch"`
		FirstSeen time.Time `json:"firstSeen"`
		LastSeen  time.Time `json:"lastSeen"`
	} `json:"sessions"`
}

type pluginAnswer struct {
	RunID  string `json:"runId"`
	Status string `json:"status"`
	Final  bool   `json:"final"`
	Lines  []struct {
		Family   string         `json:"family"`
		Item     string         `json:"item"`
		Amount   string         `json:"amount"`
		Currency string         `json:"currency"`
		From     time.Time      `json:"from"`
		To       time.Time      `json:"to"`
		Details  map[string]any `json:"details"`
	} `json:"lines"`
	Error      string `json:"error"`
	RetryAfter string `json:"retryAfter"`
	err        error
	retry      time.Duration
	fault      bool
}

func loadPluginRuns(ctx context.Context, tx pgx.Tx, ids []string) (map[string]pluginRun, error) {
	out := map[string]pluginRun{}
	rows, err := tx.Query(ctx, `SELECT r.id, r.tenant_id, t.name, r.labels, r.spec, r.state, r.created_at, r.finished_at
		FROM runs r JOIN tenants t ON t.id = r.tenant_id WHERE r.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r pluginRun
		var spec map[string]any
		if err := rows.Scan(&r.RunID, &r.TenantID, &r.TenantName, &r.Labels, &spec, &r.State, &r.Window.From, &r.Window.To); err != nil {
			return nil, err
		}
		if r.Labels == nil {
			r.Labels = map[string]string{}
		}
		if workload, ok := spec["workload"].(map[string]any); ok {
			if adapter, ok := workload["adapter"].(string); ok {
				r.Adapter = adapter
			}
		}
		r.Terminal = terminal(r.State)
		r.Placements = make([]struct {
			Epoch int        `json:"epoch"`
			From  time.Time  `json:"from"`
			To    *time.Time `json:"to"`
		}, 0)
		r.Sessions = make([]struct {
			ID        string    `json:"id"`
			Epoch     int       `json:"epoch"`
			FirstSeen time.Time `json:"firstSeen"`
			LastSeen  time.Time `json:"lastSeen"`
		}, 0)
		out[r.RunID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT run_id, epoch, created_at, ended_at FROM placements WHERE run_id = ANY($1) ORDER BY run_id, epoch`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p struct {
			Epoch int        `json:"epoch"`
			From  time.Time  `json:"from"`
			To    *time.Time `json:"to"`
		}
		if err := rows.Scan(&id, &p.Epoch, &p.From, &p.To); err != nil {
			rows.Close()
			return nil, err
		}
		r := out[id]
		r.Placements = append(r.Placements, p)
		out[id] = r
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT run_id, session_id, epoch, first_seen, last_seen FROM run_sessions WHERE run_id = ANY($1) ORDER BY run_id, epoch, session_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p struct {
			ID        string    `json:"id"`
			Epoch     int       `json:"epoch"`
			FirstSeen time.Time `json:"firstSeen"`
			LastSeen  time.Time `json:"lastSeen"`
		}
		if err := rows.Scan(&id, &p.ID, &p.Epoch, &p.FirstSeen, &p.LastSeen); err != nil {
			return nil, err
		}
		r := out[id]
		r.Sessions = append(r.Sessions, p)
		out[id] = r
	}
	return out, rows.Err()
}

func (s *Server) initCostPlugins() {
	s.pluginsOnce.Do(func() {
		for _, cfg := range s.cfg.Costs.Plugins {
			if cfg.Timeout <= 0 {
				cfg.Timeout = 30 * time.Second
			}
			if cfg.MaxBatch <= 0 {
				cfg.MaxBatch = 200
			}
			s.plugins = append(s.plugins, &costPlugin{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}}})
		}
	})
}

func (s *Server) describeCostPlugins(ctx context.Context) {
	for _, p := range s.plugins {
		var d pluginDescribe
		err := p.request(ctx, http.MethodGet, "/v1/describe", nil, &d)
		if err == nil && !slices.Contains(d.Protocol, 1) {
			err = errors.New("protocol 1 unsupported")
		}
		if err == nil && d.Name != p.cfg.Name {
			err = fmt.Errorf("describe name %q does not match %q", d.Name, p.cfg.Name)
		}
		var previous time.Duration
		if err == nil {
			for _, raw := range d.Settle {
				value, parseErr := time.ParseDuration(raw)
				if parseErr != nil || value <= previous {
					err = fmt.Errorf("invalid describe settle duration %q", raw)
					break
				}
				previous = value
			}
		}
		p.mu.Lock()
		if err == nil {
			p.desc, p.usable = d, true
		} else {
			p.usable = false
		}
		p.mu.Unlock()
		p.health(s, err)
	}
}

func (p *costPlugin) health(s *Server, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		if !p.failing.IsZero() {
			s.log.Info("cost plugin recovered", "plugin", p.cfg.Name)
		}
		p.failing, p.lastError = time.Time{}, ""
	} else {
		if p.failing.IsZero() {
			p.failing = time.Now()
			s.log.Warn("cost plugin failing", "plugin", p.cfg.Name, "err", err)
		}
		p.lastError = err.Error()
	}
}

type pluginHTTPError struct {
	status int
	retry  time.Duration
}

func (e pluginHTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }

func (p *costPlugin) request(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.cfg.URL, "/")+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var token string
	if p.cfg.TokenFile != "" {
		b, err := os.ReadFile(p.cfg.TokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	} else if p.cfg.TokenEnv != "" {
		token = os.Getenv(p.cfg.TokenEnv)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var after time.Duration
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			after = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			after = time.Until(at)
		}
		return pluginHTTPError{resp.StatusCode, after}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, pluginBodyLimit+1))
	if err != nil {
		return err
	}
	if len(b) > pluginBodyLimit {
		return errors.New("response exceeds 16 MiB")
	}
	return json.Unmarshal(b, out)
}

func loadPluginDue(ctx context.Context, tx pgx.Tx, runs []string, now time.Time) (map[string]map[string]bool, error) {
	due := map[string]map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT c.run_id, c.source, c.status, c.next_at, p.reason
		FROM cost_sources c JOIN cost_pending p ON p.run_id = c.run_id WHERE c.run_id = ANY($1)`, runs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, source, status string
		var reason string
		var next *time.Time
		if err := rows.Scan(&id, &source, &status, &next, &reason); err != nil {
			return nil, err
		}
		if due[source] == nil {
			due[source] = map[string]bool{}
		}
		due[source][id] = status != "final" &&
			(next == nil || !now.Before(*next) || strings.HasPrefix(reason, "state:"))
	}
	return due, rows.Err()
}

func (s *Server) reportCostPlugins(ctx context.Context, runs map[string]pluginRun, due map[string]map[string]bool, evals map[string]*computeEval, now time.Time) error {
	if len(runs) == 0 {
		return nil
	}
	s.initCostPlugins()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, p := range s.plugins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.mu.RLock()
			d, usable := p.desc, p.usable
			p.mu.RUnlock()
			limit := p.cfg.MaxBatch
			if usable && d.MaxBatch > 0 {
				limit = min(limit, d.MaxBatch)
			}
			ids := make([]string, 0, len(runs))
			for id := range runs {
				if allowed, exists := due[p.cfg.Name][id]; !exists || allowed {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			if len(ids) == 0 {
				return
			}
			var failed error
			for chunk := range slices.Chunk(ids, limit) {
				if ctx.Err() != nil {
					return
				}
				answers := map[string]pluginAnswer{}
				batch := make([]pluginRun, 0, len(chunk))
				for _, id := range chunk {
					batch = append(batch, runs[id])
				}
				var response struct {
					Protocol int            `json:"protocol"`
					Runs     []pluginAnswer `json:"runs"`
				}
				var err error
				if !usable {
					err = errors.New("describe unavailable or incompatible")
				} else {
					err = p.request(ctx, http.MethodPost, "/v1/costs", map[string]any{"protocol": 1, "requestId": idsNewCost(), "sentAt": time.Now().UTC(), "runs": batch}, &response)
					if err == nil && response.Protocol != 1 {
						err = errors.New("unexpected protocol")
					}
				}
				if err != nil {
					if failed == nil {
						failed = err
					}
					var he pluginHTTPError
					_ = errors.As(err, &he)
					if he.status >= 400 && he.status < 500 && he.status != 429 {
						s.log.Error("cost plugin configuration fault", "plugin", p.cfg.Name, "err", err)
					}
					for _, id := range chunk {
						answers[id] = pluginAnswer{err: err, retry: he.retry, fault: he.status >= 400 && he.status < 500 && he.status != 429}
					}
				} else {
					seen := map[string]bool{}
					for _, a := range response.Runs {
						if !slices.Contains(chunk, a.RunID) {
							s.log.Warn("cost plugin returned foreign run", "plugin", p.cfg.Name, "run", a.RunID)
							continue
						}
						if seen[a.RunID] {
							answers[a.RunID] = pluginAnswer{err: errors.New("duplicate run response")}
							continue
						}
						seen[a.RunID] = true
						items := map[string]bool{}
						if a.Status == "ok" {
							for _, l := range a.Lines {
								if items[l.Item] || !costAmount.MatchString(l.Amount) || l.Family == "" || l.Currency == "" || l.From.IsZero() || l.To.IsZero() || l.To.Before(l.From) {
									a.err = errors.New("invalid or duplicate cost line")
									break
								}
								items[l.Item] = true
							}
						} else if a.Status == "error" {
							a.err = errors.New(a.Error)
							if a.Error == "" {
								a.err = errors.New("plugin run error")
							}
							a.retry, _ = time.ParseDuration(a.RetryAfter)
						} else {
							a.err = errors.New("invalid run status")
						}
						if old, duplicate := answers[a.RunID]; duplicate && old.err != nil && old.err.Error() == "duplicate run response" {
							continue
						}
						answers[a.RunID] = a
					}
					for _, id := range chunk {
						if !seen[id] {
							answers[id] = pluginAnswer{err: errors.New("missing run response")}
						}
					}
				}
				for _, a := range answers {
					if a.err != nil && failed == nil {
						failed = a.err
					}
				}
				writeErr := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, chunk); err != nil {
						return err
					}
					rows, err := tx.Query(ctx, `SELECT run_id FROM cost_pending WHERE run_id = ANY($1) AND claimed_by = $2 ORDER BY run_id FOR UPDATE`, chunk, s.id)
					if err != nil {
						return err
					}
					held, err := pgx.CollectRows(rows, pgx.RowTo[string])
					if err != nil {
						return err
					}
					for _, id := range held {
						if err := s.writePluginCost(ctx, tx, p.cfg, id, evals[id], answers[id], now); err != nil {
							return err
						}
					}
					return nil
				})
				if writeErr != nil {
					mu.Lock()
					first = cmp.Or(first, writeErr)
					mu.Unlock()
				}
			}
			if ctx.Err() == nil {
				p.health(s, failed)
			}
		}()
	}
	wg.Wait()
	return first
}

func idsNewCost() string { return ids.New("cq") }

func (s *Server) writePluginCost(ctx context.Context, tx pgx.Tx, cfg CostPluginConfig, id string, e *computeEval, a pluginAnswer, now time.Time) error {
	if e == nil {
		return nil
	}
	var attempts int
	var left *int
	var answered *time.Time
	var previousNext *time.Time
	err := tx.QueryRow(ctx, `SELECT attempts, settles_left, answered_at, next_at FROM cost_sources WHERE run_id = $1 AND source = $2`, id, cfg.Name).Scan(&attempts, &left, &answered, &previousNext)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	settle := cfg.Settle
	if settle == nil {
		for _, p := range s.plugins {
			if p.cfg.Name == cfg.Name {
				p.mu.RLock()
				for _, d := range p.desc.Settle {
					if v, err := time.ParseDuration(d); err == nil && v > 0 {
						settle = append(settle, v)
					}
				}
				p.mu.RUnlock()
				break
			}
		}
	}
	if settle == nil {
		settle = s.cfg.Costs.Settle
	}
	status, last := "ok", ""
	var next *time.Time
	if a.err != nil {
		status, last = "incomplete", a.err.Error()
		attempts++
		if a.fault {
			a.retry = s.cfg.Costs.BackoffMax
		} else if a.retry <= 0 {
			a.retry = min(s.cfg.Costs.Backoff*time.Duration(1<<min(attempts-1, 20)), s.cfg.Costs.BackoffMax)
			a.retry = time.Duration(float64(a.retry) * (0.8 + rand.Float64()*0.4))
		}
		base := e.FinishedAt
		if base == nil && (e.State == StateStopped || e.State == StateLost) {
			base = &e.StateAt
		}
		if base == nil || now.Before(base.Add(s.cfg.Costs.SettleGiveUp)) {
			t := now.Add(a.retry)
			next = &t
		}
	} else {
		lines := make([]costReport, 0, len(a.Lines))
		for _, l := range a.Lines {
			lines = append(lines, costReport{Family: l.Family, Item: l.Item, Amount: l.Amount, Currency: l.Currency, From: l.From, To: l.To, Details: l.Details})
		}
		if terminal(e.State) || e.State == StateStopped || e.State == StateLost {
			if left == nil {
				n := len(settle)
				left = &n
			}
			base := e.FinishedAt
			if base == nil {
				base = &e.StateAt
			}
			if answered != nil && previousNext != nil && !now.Before(*previousNext) && *left > 0 {
				index := len(settle) - *left
				if !now.Before(base.Add(settle[index])) {
					*left--
				}
			}
			if terminal(e.State) && (a.Final || *left == 0) {
				status = "final"
			} else if *left > 0 {
				t := base.Add(settle[len(settle)-*left])
				if !t.After(now) {
					t = now.Add(s.cfg.Costs.Backoff)
				}
				next = &t
			}
		} else {
			left = nil
		}
		for i := range lines {
			lines[i].Final = status == "final"
		}
		if err := replaceCostLines(ctx, tx, e.TenantID, id, cfg.Name, lines); err != nil {
			return err
		}
		attempts = 0
	}
	_, err = tx.Exec(ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, answered_at, attempts, next_at, settles_left, last_error)
		VALUES ($1,$2,$3,$4,CASE WHEN $5 THEN now() END,$6,$7,$8,$9)
		ON CONFLICT (run_id,source) DO UPDATE SET status = EXCLUDED.status, answered_at = coalesce(EXCLUDED.answered_at,cost_sources.answered_at), attempts = EXCLUDED.attempts, next_at = EXCLUDED.next_at, settles_left = EXCLUDED.settles_left, last_error = EXCLUDED.last_error`, id, e.TenantID, cfg.Name, status, a.err == nil, attempts, next, left, last)
	return err
}
