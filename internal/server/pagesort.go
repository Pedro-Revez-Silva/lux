package server

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Paged lists (GET /v1/hosts, /v1/runs, /v1/pools/{name}/events with
// sort or a cursor): keyset pages over (sort value, id), in any sortable
// column's order. A response's next, prev and page cursors go back as
// ?next=, ?prev= and ?at=. Missing values (NULL) sort last in both directions: the
// order is (value IS NULL), value, id, all in the page's direction but the
// first. A cursor names the row a page starts after (after), ends before
// (before) or starts at (from: a poll re-reading the page it is on), with
// the sort it was made in and the clock its computed values were read at,
// so paging through values that grow with time (a runtime) stays stable.

// sortKey is one sortable column of a list: the SQL of its value (with
// {now} where the clock goes) and its type, to cast a cursor's value back.
type sortKey struct {
	expr string
	cast string // text, float8, timestamptz, bigint
	// first: the direction of a first click (text ascending, numbers and
	// times descending), the default when a request names no dir.
	first string
	// notNull: the value is never NULL, so the order and the keyset are
	// plain (value, id), which an index on them serves.
	notNull bool
	// runOnly: the value needs no join (runs: read from runs alone).
	runOnly bool
}

// sqlArgs collects one statement's placeholders.
type sqlArgs struct{ list []any }

func (a *sqlArgs) arg(v any) string {
	a.list = append(a.list, v)
	return "$" + strconv.Itoa(len(a.list))
}

// fork is a copy for another statement sharing the placeholders so far.
func (a *sqlArgs) fork() *sqlArgs { return &sqlArgs{slices.Clone(a.list)} }

// pageCursor is a cursor's content. V is the sort value as Postgres
// prints it (nil: NULL), At the clock the page was read at.
type pageCursor struct {
	Sort string  `json:"s"`
	Dir  string  `json:"d"`
	V    *string `json:"v"`
	ID   string  `json:"i"`
	At   string  `json:"t,omitempty"`
}

func (c pageCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*pageCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c pageCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return nil, errf(http.StatusBadRequest, "bad_request", "not a cursor")
	}
	return &c, nil
}

// PageQuery is the paging of a list request.
type PageQuery struct {
	Sort   string `query:"sort" doc:"Sort by this column (the list's sort keys); with it, or with a cursor, the list is paged."`
	Dir    string `query:"dir" enum:"asc,desc," doc:"asc or desc; default: the column's first direction (text ascending, numbers and times descending). Missing values sort last either way."`
	After  string `query:"next" doc:"The page after this one: a page's next cursor."`
	Before string `query:"prev" doc:"The page before this one: a page's prev cursor."`
	From   string `query:"at" doc:"This page again, from its first row: a page's page cursor (a refresh that stays on the page)."`
}

// cursorParam names each paging mode's query parameter.
var cursorParam = map[string]string{"after": "next", "before": "prev", "from": "at"}

// paging is a resolved PageQuery.
type paging struct {
	key    string
	dir    string
	sk     sortKey
	cursor *pageCursor
	mode   string // "", after, before, from
	limit  int
	// idCast: the id column's type, when not text (an event's bigint id).
	idCast string
}

// resolvePaging checks q against a list's sort keys. ok is false when the
// request asks for no paging (no sort and no cursor): the list's own
// unpaged behaviour applies.
func resolvePaging(q PageQuery, keys map[string]sortKey, def string, limit string, defLimit, maxLimit int) (*paging, bool, error) {
	var raw, mode string
	for _, m := range []struct{ name, v string }{{"after", q.After}, {"before", q.Before}, {"from", q.From}} {
		// Named in errors as the client sent them.
		if m.v == "" {
			continue
		}
		if raw != "" {
			return nil, false, errf(http.StatusBadRequest, "bad_request", "at most one of next, prev and at")
		}
		raw, mode = m.v, m.name
	}
	if q.Sort == "" && raw == "" {
		return nil, false, nil
	}
	p := &paging{key: q.Sort, dir: q.Dir, mode: mode, limit: defLimit}
	if raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			return nil, false, errf(http.StatusBadRequest, "bad_request", "%s: not a cursor", cursorParam[mode])
		}
		if (p.key != "" && p.key != c.Sort) || (p.dir != "" && p.dir != c.Dir) {
			return nil, false, errf(http.StatusBadRequest, "bad_request", "%s: a cursor of another sort", cursorParam[mode])
		}
		p.cursor, p.key, p.dir = c, c.Sort, c.Dir
	}
	if p.key == "" {
		p.key = def
	}
	sk, found := keys[p.key]
	if !found {
		return nil, false, errf(http.StatusBadRequest, "bad_request", "sort: one of %s", sortKeysDoc(keys))
	}
	p.sk = sk
	if p.dir == "" {
		p.dir = sk.first
	}
	if p.dir != "asc" && p.dir != "desc" {
		return nil, false, errf(http.StatusBadRequest, "bad_request", "dir: asc or desc")
	}
	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 || n > maxLimit {
			return nil, false, errf(http.StatusBadRequest, "bad_request", "limit: 1 to %d", maxLimit)
		}
		p.limit = n
	}
	return p, true, nil
}

// expr is the sort value's SQL with the clock filled in: the cursor's for a
// page read from one, else now().
func (p *paging) expr(arg func(any) string) string {
	if !strings.Contains(p.sk.expr, "{now}") {
		return "(" + p.sk.expr + ")"
	}
	now := "now()"
	if p.cursor != nil && p.cursor.At != "" {
		now = arg(p.cursor.At) + "::timestamptz"
	}
	return "(" + strings.ReplaceAll(p.sk.expr, "{now}", now) + ")"
}

// order is the ORDER BY of the page's rows; reversed reads backwards (for
// before), and the caller reverses the rows it gets.
func (p *paging) order(expr, id string, reversed bool) string {
	dir := p.dir
	nulls := "ASC"
	if reversed {
		dir = map[string]string{"asc": "desc", "desc": "asc"}[dir]
		nulls = "DESC"
	}
	if p.sk.notNull {
		return expr + " " + dir + ", " + id + " " + dir
	}
	return "(" + expr + " IS NULL) " + nulls + ", " + expr + " " + dir + ", " + id + " " + dir
}

// where is the keyset condition of the cursor ("true" without one).
func (p *paging) where(expr, id string, arg func(any) string) string {
	c := p.cursor
	if c == nil {
		return "true"
	}
	gt, ge := ">", ">="
	if p.dir == "desc" {
		gt, ge = "<", "<="
	}
	lt := map[string]string{">": "<", "<": ">"}[gt]
	idOp := gt
	if p.mode == "from" {
		idOp = ge
	}
	idArg := arg(c.ID)
	if p.idCast != "" {
		idArg += "::" + p.idCast
	}
	if p.sk.notNull && c.V != nil {
		// A row comparison, which an index on (value, id) serves.
		v := arg(*c.V) + "::" + p.sk.cast
		op := idOp
		if p.mode == "before" {
			op = lt
		}
		return "((" + expr + ", " + id + ") " + op + " (" + v + ", " + idArg + "))"
	}
	if p.mode == "before" {
		if c.V == nil {
			return "(" + expr + " IS NOT NULL OR " + id + " " + lt + " " + idArg + ")"
		}
		v := arg(*c.V) + "::" + p.sk.cast
		return "(" + expr + " IS NOT NULL AND (" + expr + " " + lt + " " + v + " OR (" + expr + " = " + v + " AND " + id + " " + lt + " " + idArg + ")))"
	}
	if c.V == nil {
		return "(" + expr + " IS NULL AND " + id + " " + idOp + " " + idArg + ")"
	}
	v := arg(*c.V) + "::" + p.sk.cast
	return "(" + expr + " IS NULL OR " + expr + " " + gt + " " + v + " OR (" + expr + " = " + v + " AND " + id + " " + idOp + " " + idArg + "))"
}

// keyRow is a page row's id and sort value, as the page query returns them.
type keyRow struct {
	ID string
	V  *string
}

// pageLinks trims the extra row read to learn whether there is a next
// page, restores the order of a before page, and makes the page's cursors:
// next, prev (hasBefore says whether rows precede the page's first) and
// its own (from=, to read it again in place).
func (p *paging) pageLinks(rows []keyRow, at string, hasBefore func(first keyRow) (bool, error)) (page []keyRow, next, prev, self string, err error) {
	more := len(rows) > p.limit
	if more {
		rows = rows[:p.limit]
	}
	var before bool
	if p.mode == "before" {
		slices.Reverse(rows)
		// Read backwards: the extra row was before the page, and the
		// cursor's row follows it.
		before, more = more, true
	}
	cur := func(r keyRow) string {
		return pageCursor{Sort: p.key, Dir: p.dir, V: r.V, ID: r.ID, At: at}.encode()
	}
	if len(rows) == 0 {
		return rows, "", "", "", nil
	}
	if p.mode != "before" {
		if before, err = hasBefore(rows[0]); err != nil {
			return nil, "", "", "", err
		}
	}
	if more {
		next = cur(rows[len(rows)-1])
	}
	if before {
		prev = cur(rows[0])
	}
	return rows, next, prev, cur(rows[0]), nil
}

// beforeWhere: the rows ahead of first in the page's order.
func (p *paging) beforeWhere(expr, id string, first keyRow, arg func(any) string) string {
	anchor := &paging{sk: p.sk, dir: p.dir, mode: "before", idCast: p.idCast, cursor: &pageCursor{V: first.V, ID: first.ID}}
	return anchor.where(expr, id, arg)
}

// sortKeysDoc lists a list's sort keys, for its errors and documentation.
func sortKeysDoc(keys map[string]sortKey) string {
	return strings.Join(slices.Sorted(maps.Keys(keys)), ", ")
}
