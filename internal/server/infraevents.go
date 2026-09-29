package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Pool and host events (migration 031): what happened to a pool or a host,
// written in the transaction of the change each records. Type names carry
// their table's prefix.
//
// Each pool's and each host's events are a stream, and every write to one
// takes that stream's transaction-scoped advisory lock (lockStream), keyed
// by table and owner id, so unrelated pools and hosts never wait on each
// other. An append takes it shared: appends never wait on each other, only
// on a fold. A fold (collapse) takes it exclusive before it reads the
// stream's latest events, so no other write to the stream is in flight
// uncommitted while it decides, and none can land until it commits: under
// READ COMMITTED it would otherwise fold across an event another
// transaction has written but not yet committed, and two writers could both
// write a "first" failure.
//
// Lock order, for every transaction that writes events:
//
//  0. the pool-name advisory lock, keyed by owner and name (ChangePool):
//     serialises writes to one pool, including its creation, when there
//     is no row yet to lock;
//  1. pool rows, FOR NO KEY UPDATE (ChangePool, deletePool; an event's
//     foreign key takes KEY SHARE on its pool, which NO KEY UPDATE does
//     not conflict with, so appending to a pool never waits on its edit);
//  2. runs, FOR UPDATE in id order (lockReaperRuns, the scheduler);
//  3. the cost-host advisory locks, in host id order (lockCostHost);
//  4. host rows, FOR NO KEY UPDATE, in id order;
//  5. event streams, last: no row or advisory lock is requested after one.
//
// Where each writer takes its stream locks, after every other lock:
//
//   - ChangePool (putPool, deletePool, luxd admin create-pool): its one
//     event after change returns; deletePool drains inside change.
//   - drainHosts (drainHost, deletePool, drainForScaleDown, hostEvicting):
//     host.drain_requested after its Runs, hosts and placement stops;
//     hostEvicting's pool.spot_interrupted after that. drainHostsLater
//     leaves them to a caller that locks more (drainIfOutdated).
//   - registerHost: host.registered, pool.host_registered, host.ready and
//     an outdated drain's event at the end of its first transaction; its
//     reconciliation's placement_ended events after all its Runs and hosts.
//   - heartbeat: host.ready and an outdated drain's event at its end.
//   - reapLeases, reapHosts: host.lost and placement_ended once every Run,
//     cost-host lock and host is held (laterEvents).
//   - placementExited (a runner's status report): host.placement_ended
//     after the Run, its placement, host row and any auto-resume.
//   - the scheduler (assign): Runs are locked up front (SKIP LOCKED) and
//     hosts by cost-host lock before the first Run is placed; each Run's
//     events are then written as it is placed, and the rest of the batch
//     writes only rows of Runs it already holds (their placements,
//     messages, cost rows), never a lock another writer takes first.
//   - launch: pool.scale_up and pool.launch_requested (folds) after the
//     host row it creates; pool.launch_failed (fold) after terminating that
//     host; pool.host_launched after recording the instance id.
//   - recordProviderID, terminateRequested, terminateTx, providerError:
//     after their one host row (terminateTx: its copies and token too).
//
// A fold takes its stream exclusive after every other lock its transaction
// takes (launch writes its host row, then folds; a failed launch
// terminates its host, then folds). An exclusive request waits for every
// shared holder to commit, so by then the folding transaction holds only
// rows no appender waits for: a host it has just created, or one whose
// launch failed, and that host's token and copies. Since no appender
// requests another lock after its first stream lock but another stream
// (the scheduler's next Run), an appender queued behind a waiting fold is
// never what the fold's shared holders wait on, except through such a
// queue position, which Postgres's deadlock check resolves after
// deadlock_timeout by granting the shared lock ahead of the fold. A fold
// waits for shared holders to commit: the scheduler's placement
// transaction holds its pools' and hosts' streams shared until it commits,
// so a failing pool's fold waits out at most one scheduler batch, and a
// batch waits at most for one fold (a few statements, once per failing
// provider call).
const (
	evScaleUp          = "pool.scale_up"
	evLaunchRequested  = "pool.launch_requested"
	evLaunchFailed     = "pool.launch_failed"
	evHostLaunched     = "pool.host_launched"
	evHostRegistered   = "pool.host_registered"
	evHostReleased     = "pool.host_released"
	evSpotInterrupted  = "pool.spot_interrupted"
	evConfigChanged    = "pool.config_changed"
	evRetired          = "pool.retired"
	evRestored         = "pool.restored"
	evPlacement        = "pool.placement"
	evPoolProviderErr  = "pool.provider_error"
	evRegistered       = "host.registered"
	evReady            = "host.ready"
	evPlacementAssign  = "host.placement_assigned"
	evPlacementEnded   = "host.placement_ended"
	evDrainRequested   = "host.drain_requested"
	evLost             = "host.lost"
	evTerminateRequest = "host.terminate_requested"
	evTerminated       = "host.terminated"
	evHostProviderErr  = "host.provider_error"
)

// poolRetry: the events a pool writes on every provisioner tick while a
// launch keeps failing. A repeat within an unbroken run of them is folded
// into the earlier row (see collapse) instead of adding one.
var poolRetry = []string{evScaleUp, evLaunchRequested, evLaunchFailed, evPoolProviderErr}

// hostRetry: a terminate the provider keeps refusing.
var hostRetry = []string{evHostProviderErr}

// lockStream takes an owner's event-stream lock (see the top of this file):
// shared to append, exclusive to fold.
func lockStream(ctx context.Context, tx pgx.Tx, t eventTable, owner string, exclusive bool) error {
	fn := "pg_advisory_xact_lock_shared"
	if exclusive {
		fn = "pg_advisory_xact_lock"
	}
	_, err := tx.Exec(ctx, `SELECT `+fn+`(hashtextextended($1 || ':' || $2, 0))`, t.table, owner)
	return err
}

func poolEvent(ctx context.Context, tx pgx.Tx, poolID, typ string, data map[string]any) error {
	if err := lockStream(ctx, tx, poolEvents, poolID, false); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO pool_events (tenant_id, pool_id, type, data)
		SELECT tenant_id, id, $2, $3 FROM pools WHERE id = $1`, poolID, typ, nonNilData(data))
	return err
}

// hostPoolEvent records an event on the pool a host belongs to: the pool
// of its owner (a platform host's is the platform's) and its pool's name.
// None when that pool has no row (a static pool only its host tokens name).
func hostPoolEvent(ctx context.Context, tx pgx.Tx, hostID, typ string, data map[string]any) error {
	var poolID string
	err := tx.QueryRow(ctx, `SELECT p.id FROM hosts h
		JOIN pools p ON p.name = h.pool AND p.tenant_id IS NOT DISTINCT FROM h.tenant_id
		WHERE h.id = $1`, hostID).Scan(&poolID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return poolEvent(ctx, tx, poolID, typ, data)
}

func hostEvent(ctx context.Context, tx pgx.Tx, hostID, typ string, data map[string]any) error {
	if err := lockStream(ctx, tx, hostEvents, hostID, false); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO host_events (tenant_id, host_id, type, data)
		SELECT tenant_id, id, $2, $3 FROM hosts WHERE id = $1`, hostID, typ, nonNilData(data))
	return err
}

// laterEvents holds appends a transaction decides on while it still has
// rows to lock, to write once it has locked them all (event streams come
// last). Written in the order they were added.
type laterEvents []func() error

func (l *laterEvents) host(ctx context.Context, tx pgx.Tx, hostID, typ string, data map[string]any) {
	*l = append(*l, func() error { return hostEvent(ctx, tx, hostID, typ, data) })
}

func (l *laterEvents) hostPool(ctx context.Context, tx pgx.Tx, hostID, typ string, data map[string]any) {
	*l = append(*l, func() error { return hostPoolEvent(ctx, tx, hostID, typ, data) })
}

func (l laterEvents) write() error {
	for _, w := range l {
		if err := w(); err != nil {
			return err
		}
	}
	return nil
}

// poolRepeatEvent records a pool event that a stuck pool repeats every
// tick. It folds into the latest event of its type when that one has the
// same data (but for the keys in volatile, which take the new values) and
// every pool event since is one of poolRetry; for a scale-up or a launch
// request, only once a launch has failed since (two launches in one pass
// are two rows).
func poolRepeatEvent(ctx context.Context, tx pgx.Tx, poolID, typ string, data map[string]any, volatile ...string) error {
	folded, err := collapse(ctx, tx, poolEvents, poolID, typ, data, volatile, poolRetry, typ == evScaleUp || typ == evLaunchRequested)
	if err != nil || folded {
		return err
	}
	return poolEvent(ctx, tx, poolID, typ, data)
}

// hostRepeatEvent is poolRepeatEvent for a host's provider errors.
func hostRepeatEvent(ctx context.Context, tx pgx.Tx, hostID, typ string, data map[string]any) error {
	folded, err := collapse(ctx, tx, hostEvents, hostID, typ, data, nil, hostRetry, false)
	if err != nil || folded {
		return err
	}
	return hostEvent(ctx, tx, hostID, typ, data)
}

// eventTable names one of the two event tables and its owner column.
type eventTable struct{ table, owner string }

var (
	poolEvents = eventTable{"pool_events", "pool_id"}
	hostEvents = eventTable{"host_events", "host_id"}
)

// foldWindow is how many of an owner's latest events a fold looks at. A
// failing provisioner pass stops at its first failed launch, so it writes
// at most a list error, a scale-up, a launch request and its failure,
// plus whatever else happened to the pool since. Eight is a bounded,
// best-effort compaction window, not a guarantee: a repeat whose earlier
// event lies outside it becomes another row, which is still accurate.
const foldWindow = 8

// foldLookup reads an owner's latest events, newest first, through its
// (owner, id) index: $1 owner, $2 volatile keys, $3 the new data, $4 the
// window. same: the data matches but for the volatile keys. The row bound
// is always true; it is there because only that index can serve it, where
// with the owner alone the planner may walk the primary key backwards and
// filter every newer event of other owners.
func foldLookup(t eventTable) string {
	return `SELECT id, type, data - $2::text[] = $3::jsonb - $2::text[] AS same FROM ` + t.table + `
		WHERE ` + t.owner + ` = $1 AND (` + t.owner + `, id) <= ($1, 9223372036854775807)
		ORDER BY ` + t.owner + ` DESC, id DESC LIMIT $4`
}

// collapse bumps the count of the event typ would repeat, if any (see
// poolRepeatEvent), and reports whether it did. It holds the owner's
// stream exclusively from before its read (see the top of this file), so
// what it reads as the latest events stays the latest until it commits.
func collapse(ctx context.Context, tx pgx.Tx, t eventTable, owner, typ string, data map[string]any, volatile, retry []string, afterFailure bool) (bool, error) {
	if err := lockStream(ctx, tx, t, owner, true); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, foldLookup(t), owner, nonNil(volatile), nonNilData(data), foldWindow)
	if err != nil {
		return false, err
	}
	type latest struct {
		ID   int64
		Type string
		Same bool
	}
	evs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[latest])
	if err != nil {
		return false, err
	}
	// Newest first: every event down to the latest of typ must be part of
	// the retry loop, and for afterFailure one of them a launch failure.
	failed := false
	for _, e := range evs {
		if e.Type == typ {
			if !e.Same || (afterFailure && !failed) {
				return false, nil
			}
			_, err = tx.Exec(ctx, `UPDATE `+t.table+` SET count = count + 1, last_at = now(), data = $2 WHERE id = $1`, e.ID, nonNilData(data))
			return err == nil, err
		}
		if !slices.Contains(retry, e.Type) {
			return false, nil
		}
		failed = failed || e.Type == evLaunchFailed
	}
	return false, nil
}

func nonNilData(d map[string]any) map[string]any {
	if d == nil {
		return map[string]any{}
	}
	return d
}

// ChangePool runs change, which creates, updates or retires the pool
// tenantID/name in tx, and records what it changed (nothing when nothing
// did). A pool removed is retired, keeping its id and its events; set again
// it is restored. So its history shows where it went and came back, those
// changes are pool.retired and pool.restored; any other, pool.config_changed.
func ChangePool(ctx context.Context, tx pgx.Tx, tenantID *string, name string, change func() error) error {
	// Before the baseline read: a pool with no row yet has nothing for FOR
	// NO KEY UPDATE to lock, so two first writes would both see none and
	// both record "created".
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pool-name:' || coalesce($1::text, '') || '/' || $2, 0))`, tenantID, name); err != nil {
		return err
	}
	before, err := poolSettings(ctx, tx, tenantID, name)
	if err != nil {
		return err
	}
	if err := change(); err != nil {
		return err
	}
	after, err := poolSettings(ctx, tx, tenantID, name)
	if err != nil || after == nil {
		return err
	}
	// Every field either side has: a template key removed is old→null.
	changes := map[string]any{}
	keys := maps.Clone(after.fields)
	if before != nil {
		maps.Copy(keys, before.fields)
	}
	for k := range keys {
		var old any
		if before != nil {
			old = before.fields[k]
		}
		if v := after.fields[k]; !reflect.DeepEqual(old, v) {
			changes[k] = map[string]any{"old": old, "new": v}
		}
	}
	if len(changes) == 0 {
		return nil
	}
	typ := evConfigChanged
	if before != nil && before.fields["retired"] != after.fields["retired"] {
		typ = evRestored
		if after.fields["retired"] == true {
			typ = evRetired
		}
	}
	return poolEvent(ctx, tx, after.id, typ, map[string]any{"created": before == nil, "changes": changes})
}

type poolSnapshot struct {
	id     string
	fields map[string]any
}

// poolSettings is a pool's settings as config_changed reports them, one
// value per field, template keys each on their own (template.region, ...).
// A pool holds no secrets: a template is where and what to launch (region,
// launch template, subnets, tags), and credentials are luxd's own.
func poolSettings(ctx context.Context, tx pgx.Tx, tenantID *string, name string) (*poolSnapshot, error) {
	var p poolSnapshot
	var provider, price, currency string
	var tmpl map[string]any
	var minH, maxH, warm, sda int
	var wwa, shared, retired bool
	err := tx.QueryRow(ctx, `SELECT id, provider, template, min_hosts, max_hosts, warm_hosts, coalesce(scale_down_after_s, 0),
			warm_while_active, shared, retired, coalesce(trim_scale(hourly_price)::text, ''), coalesce(price_currency, '')
		FROM pools WHERE tenant_id IS NOT DISTINCT FROM $1 AND name = $2 FOR NO KEY UPDATE`, tenantID, name).
		Scan(&p.id, &provider, &tmpl, &minH, &maxH, &warm, &sda, &wwa, &shared, &retired, &price, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.fields = map[string]any{"provider": provider, "minHosts": minH, "maxHosts": maxH, "warmHosts": warm,
		"scaleDownAfterSeconds": sda, "warmWhileActive": wwa, "shared": shared, "retired": retired,
		"hourlyPrice": price, "currency": currency}
	for k, v := range tmpl {
		// Through JSON, as the event stores it: 1 and 1.0 compare equal.
		b, _ := json.Marshal(v)
		var norm any
		_ = json.Unmarshal(b, &norm)
		p.fields["template."+k] = norm
	}
	return &p, nil
}

// providerErrorText is a provider's error with what differs between two
// otherwise identical failures (AWS's request id) taken out, so that
// repeats compare equal.
func providerErrorText(err error) string {
	return truncate(requestID.ReplaceAllString(err.Error(), ""), 500)
}

var requestID = regexp.MustCompile(`(?i),?\s*request ?id: [0-9a-f-]+,?`)

// LifecycleEvent is one of a pool's or a host's events.
type LifecycleEvent struct {
	ID    int64          `json:"id"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data"`
	Count int            `json:"count" doc:"How many times it happened in a row (a failure repeated on every provisioner pass); 1 for most."`
	Time  time.Time      `json:"time" doc:"When it (first) happened."`
	// LastTime: only for a repeated event.
	LastTime *time.Time `json:"lastTime,omitempty" doc:"When it last happened, if more than once."`
}

// EventPage is a page of events, newest first.
type EventPage struct {
	Before string `query:"before" doc:"Only events older than this id: the next page after a page's last event." example:"0"`
	Limit  string `query:"limit" doc:"At most this many events: 1 to 1000, default 100." example:"100"`
}

type lifecycleEventsOutput struct {
	Body struct {
		Events []LifecycleEvent `json:"events"`
	} `nameHint:"LifecycleEventList"`
}

type listPoolEventsInput struct {
	TenantQuery
	EventPage
	Name  string `path:"name" doc:"The pool's name."`
	Owner string `query:"owner" enum:"platform,tenant" doc:"Which pool of that name: the platform's, or a tenant's (the caller's, or with ?tenant= that tenant's). Omitted: a tenant's own pool, else the platform's; for an operator not narrowed with ?tenant=, a name two pools share is ambiguous (409)."`
}

type listHostEventsInput struct {
	HostPath
	TenantQuery
	EventPage
}

// seesPlatformEvents: platform pools' and hosts' events name other
// tenants' Runs, so they are an operator's, and not an operator's narrowed
// with ?tenant= (which shows what that tenant would see).
func (p Principal) seesPlatformEvents() bool { return p.Operator && p.TenantID == "" }

func (s *Server) listPoolEvents(ctx context.Context, in *listPoolEventsInput) (*lifecycleEventsOutput, error) {
	p := principal(ctx)
	var poolID string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Unless owner says which, a tenant's own pool shadows the
		// platform's of the same name, as for its Runs. An operator not
		// narrowed to a tenant sees every tenant's pool: a name two of
		// them share is ambiguous.
		rows, err := tx.Query(ctx, `SELECT id, tenant_id IS NULL FROM pools
			WHERE name = $2
			  AND CASE $3 WHEN 'platform' THEN tenant_id IS NULL
			              WHEN 'tenant' THEN tenant_id IS NOT NULL AND ($1 = '' OR tenant_id = $1)
			              ELSE $1 = '' OR tenant_id = $1 OR tenant_id IS NULL END
			ORDER BY tenant_id NULLS LAST, retired LIMIT 2`, p.TenantID, in.Name, in.Owner)
		if err != nil {
			return err
		}
		type found struct {
			ID       string
			Platform bool
		}
		pools, err := pgx.CollectRows(rows, pgx.RowToStructByPos[found])
		switch {
		case err != nil:
			return err
		case len(pools) == 0:
			return errNotFound
		case len(pools) > 1 && p.TenantID == "":
			return errf(http.StatusConflict, "ambiguous", "more than one pool is named %s: use ?owner=platform, or ?tenant=", in.Name)
		case pools[0].Platform && !p.seesPlatformEvents():
			return errf(http.StatusForbidden, "forbidden", "a platform pool's events are the operators'")
		}
		poolID = pools[0].ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.lifecycleEvents(ctx, p, poolEvents, poolID, in.EventPage)
}

func (s *Server) listHostEvents(ctx context.Context, in *listHostEventsInput) (*lifecycleEventsOutput, error) {
	p := principal(ctx)
	var hostID string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := s.resolveHost(ctx, tx, p, in.ID, true)
		if err != nil {
			return err
		}
		var platform bool
		if err := tx.QueryRow(ctx, `SELECT tenant_id IS NULL FROM hosts WHERE id = $1`, id).Scan(&platform); err != nil {
			return err
		}
		if platform && !p.seesPlatformEvents() {
			return errf(http.StatusForbidden, "forbidden", "a platform host's events are the operators'")
		}
		hostID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.lifecycleEvents(ctx, p, hostEvents, hostID, in.EventPage)
}

// lifecycleEvents reads a page of an owner's events, newest first. A
// tenant, or an operator narrowed to one, reads under that tenant's scope:
// row-level security holds even if the checks before are wrong.
func (s *Server) lifecycleEvents(ctx context.Context, p Principal, t eventTable, owner string, page EventPage) (*lifecycleEventsOutput, error) {
	limit := 100
	if n, err := strconv.Atoi(page.Limit); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	var before *int64
	if page.Before != "" {
		n, err := strconv.ParseInt(page.Before, 10, 64)
		if err != nil {
			return nil, errf(http.StatusBadRequest, "bad_request", "before: an event id")
		}
		before = &n
	}
	sc := p.scope()
	out := &lifecycleEventsOutput{}
	out.Body.Events = []LifecycleEvent{}
	err := s.db.Tx(ctx, sc, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, type, data, count, created_at, last_at FROM `+t.table+`
			WHERE `+t.owner+` = $1 AND ($2::bigint IS NULL OR id < $2) ORDER BY id DESC LIMIT $3`, owner, before, limit)
		if err != nil {
			return err
		}
		var e LifecycleEvent
		var last time.Time
		_, err = pgx.ForEachRow(rows, []any{&e.ID, &e.Type, &e.Data, &e.Count, &e.Time, &last}, func() error {
			ev := e
			if ev.Count > 1 {
				at := last
				ev.LastTime = &at
			}
			out.Body.Events = append(out.Body.Events, ev)
			return nil
		})
		return err
	})
	return out, err
}
