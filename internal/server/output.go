package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Cursor is a position in a Run's output: its placements' records in epoch
// order, then sequence. Opaque to clients ("<epoch>.<seq>"), and valid
// across moves between hosts.
type Cursor struct {
	Epoch int
	Seq   int64
}

func (c Cursor) String() string { return fmt.Sprintf("%d.%d", c.Epoch, c.Seq) }

func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	e, q, ok := strings.Cut(s, ".")
	ep, err1 := strconv.Atoi(e)
	sq, err2 := strconv.ParseInt(q, 10, 64)
	if !ok || err1 != nil || err2 != nil || ep < 0 || sq < 0 {
		return Cursor{}, fmt.Errorf("invalid cursor %q", s)
	}
	return Cursor{ep, sq}, nil
}

// OutputRecord is one SSE "record" event.
type OutputRecord struct {
	Cursor string          `json:"cursor"`
	Epoch  int             `json:"epoch"`
	Seq    int64           `json:"seq"`
	Time   int64           `json:"t"`
	Ch     string          `json:"ch" doc:"stdout, stderr, event, or server (a server's output: with servers=true or server=)."`
	Data   string          `json:"data,omitempty"`
	Event  json.RawMessage `json:"event,omitempty"`
	Server string          `json:"server,omitempty" doc:"ch=server: the server whose command wrote it."`
	Stream string          `json:"stream,omitempty" doc:"ch=server: stdout or stderr (none: its start or exit, as event)."`
}

// The other SSE events' data. Fields in key order: they were maps.
type outputGap struct {
	Epoch  int    `json:"epoch"`
	Reason string `json:"reason"`
}

type outputError struct {
	Error string `json:"error"`
}

type outputEnd struct {
	AfterEvent int64  `json:"afterEvent" doc:"The last lifecycle event sent (afterEvent)."`
	Cursor     string `json:"cursor" doc:"Where to resume (since)."`
	State      string `json:"state" doc:"The Run's state."`
}

type placementOutput struct {
	runID     string
	epoch     int
	hostID    string
	state     string
	blobKey   string
	blobLoc   string
	outputSeq *int64
}

type outputInput struct {
	RunPath
	Since      string `query:"since" doc:"Only records after this cursor (from a record or the end event)." example:"1.42"`
	Follow     string `query:"follow" doc:"true to keep streaming until the Run stops or finishes." example:"true"`
	Events     string `query:"events" doc:"true to interleave lifecycle events (lux events)." example:"true"`
	AfterEvent string `query:"afterEvent" doc:"With events, only lifecycle events after this id." example:"0"`
	Servers    string `query:"servers" doc:"true to include the output of the Run's servers (ch=server), left out by default." example:"true"`
	Server     string `query:"server" doc:"Only this server's output (ch=server records naming it)."`
}

// serveOutput is GET /v1/runs/{id}/output: the Run's output as SSE, from
// wherever it is. A live placement's records come from its host, relayed
// over the runner's WebSocket; an exited one's from S3 (or from its host if
// the upload has not finished). Lifecycle events from Postgres are
// interleaved as "lux" events.
func (s *Server) serveOutput(w http.ResponseWriter, r *http.Request, in *outputInput) error {
	p := principal(r.Context())
	runID := in.ID
	cur, err := ParseCursor(in.Since)
	if err != nil {
		return errf(http.StatusBadRequest, "bad_request", "%v", err)
	}
	follow := in.Follow == "true"
	withEvents := in.Events == "true"
	// Which records go out: servers' output only when asked for, so a
	// client that knows nothing of servers never sees it mixed in.
	wanted := func(rec proto.Record) bool {
		switch {
		case in.Server != "":
			return rec.Ch == "server" && rec.Server == in.Server
		case rec.Ch == "server":
			return in.Servers == "true"
		}
		return true
	}
	if _, err := s.loadRun(r.Context(), p.TenantID, runID, false); err != nil {
		return err
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ctx := r.Context()

	var lastEvent int64
	if after, err := strconv.ParseInt(in.AfterEvent, 10, 64); err == nil {
		lastEvent = after
	}
	send := func(event string, v any) error {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	flushEvents := func() error {
		if !withEvents {
			return nil
		}
		evs, err := s.events(ctx, p.TenantID, runID, lastEvent)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := send("lux", e); err != nil {
				return err
			}
			lastEvent = e.ID
		}
		return nil
	}

	for {
		woken := s.wakeups.next(runID) // before the reads: see wakeups
		placements, runState, err := s.placementsFrom(ctx, p.TenantID, runID, cur.Epoch)
		if err != nil {
			return send("error", outputError{err.Error()})
		}
		progressed := false
		for _, pl := range placements {
			since := int64(0)
			if pl.epoch == cur.Epoch {
				since = cur.Seq
			} else if pl.epoch < cur.Epoch {
				continue
			}
			liveNow := pl.state == "assigned" || pl.state == "starting" || pl.state == "running" || pl.state == "stopping"
			emit := func(rec proto.Record) error {
				cur = Cursor{pl.epoch, rec.Seq}
				if !wanted(rec) {
					return nil
				}
				return send("record", OutputRecord{Cursor: cur.String(), Epoch: pl.epoch, Seq: rec.Seq, Time: rec.Time, Ch: rec.Ch,
					Data: rec.Data, Event: rec.Event, Server: rec.Server, Stream: rec.Stream})
			}
			var done bool
			switch {
			case pl.blobLoc == "s3":
				err = s.streamOutputBlob(ctx, pl.blobKey, since, emit)
				done = true
			case pl.state == "lost" && pl.blobLoc != "host":
				// Its host died before saving it: gone, by design.
				_ = send("gap", outputGap{pl.epoch, "output lost with its host"})
				done = true
			case s.hub.Streaming(pl.hostID):
				done, err = s.relayOutput(ctx, pl, since, follow && liveNow, emit, flushEvents)
			case liveNow && follow:
				// No stream from the host: it is reconnecting, or it polls
				// (no live relay; its output arrives with the upload). Wait.
				done = false
			default:
				if pl.state == "exited" || pl.state == "lost" {
					_ = send("gap", outputGap{pl.epoch, "host unreachable; output not uploaded yet"})
				}
				done = !liveNow
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if liveNow && follow {
					// A hiccup on a live placement (runner reconnecting, a
					// dropped relay): keep the cursor and try again.
					done = false
				} else {
					_ = send("gap", outputGap{pl.epoch, err.Error()})
					done = true
				}
			}
			if !done {
				break
			}
			// Finished this placement: move the cursor to the next epoch.
			if cur.Epoch <= pl.epoch {
				cur = Cursor{pl.epoch + 1, 0}
			}
			progressed = true
		}
		if err := flushEvents(); err != nil {
			return nil
		}
		ended := terminal(runState) || runState == StateStopped || runState == StateLost
		if !follow || (ended && allDone(placements, cur)) {
			return send("end", outputEnd{lastEvent, cur.String(), runState})
		}
		if !progressed {
			// An event (a state change, a new placement) wakes it at once;
			// output uploads and the like are checked every half second.
			wait(ctx, woken, 500*time.Millisecond)
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}

func allDone(ps []placementOutput, cur Cursor) bool {
	for _, p := range ps {
		if p.epoch >= cur.Epoch {
			return false
		}
	}
	return true
}

func (s *Server) placementsFrom(ctx context.Context, tenantID, runID string, epoch int) ([]placementOutput, string, error) {
	var out []placementOutput
	var state string
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.epoch, p.host_id, p.state, coalesce(b.s3_key, ''), coalesce(b.location, ''), p.output_seq
			FROM placements p LEFT JOIN blobs b ON b.id = p.output_blob_id
			WHERE p.run_id = $1 AND p.epoch >= $2 ORDER BY p.epoch`, runID, epoch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			po := placementOutput{runID: runID}
			if err := rows.Scan(&po.epoch, &po.hostID, &po.state, &po.blobKey, &po.blobLoc, &po.outputSeq); err != nil {
				return err
			}
			out = append(out, po)
		}
		return rows.Err()
	})
	return out, state, err
}

// streamOutputBlob reads an uploaded output file: zstd-compressed JSON
// lines, one proto.Record each.
func (s *Server) streamOutputBlob(ctx context.Context, key string, since int64, emit func(proto.Record) error) error {
	body, _, err := s.blobs.Get(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()
	return readRecords(body, since, emit)
}

func readRecords(r io.Reader, since int64, emit func(proto.Record) error) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var rec proto.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Seq <= since {
			continue
		}
		if err := emit(rec); err != nil {
			return err
		}
	}
	return sc.Err()
}

// relayOutput asks the placement's host for records after since and relays
// them. Returns done=true when the host reported the end of the file (the
// placement exited and everything was sent).
func (s *Server) relayOutput(ctx context.Context, pl placementOutput, since int64, follow bool, emit func(proto.Record) error, tick func() error) (bool, error) {
	subID := ids.New("sub")
	ch, cancel := s.hub.Subscribe(subID)
	defer cancel()
	runID := pl.runID
	f := proto.Frame{Type: proto.MsgOutputSubscribe, RunID: runID, Epoch: pl.epoch,
		Data: proto.Marshal(proto.OutputSubscribe{SubID: subID, Since: since, Follow: follow})}
	if err := s.hub.SendLive(pl.hostID, f); err != nil {
		return false, err
	}
	defer s.hub.SendLive(pl.hostID, proto.Frame{Type: proto.MsgOutputCancel, RunID: runID, Epoch: pl.epoch,
		Data: proto.Marshal(proto.OutputSubscribe{SubID: subID})})
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
			if err := tick(); err != nil {
				return false, err
			}
			idle++
			if !s.hub.Streaming(pl.hostID) {
				return false, fmt.Errorf("host disconnected")
			}
			if !follow && idle > 30 {
				return false, fmt.Errorf("host did not answer")
			}
		case fr, ok := <-ch:
			if !ok {
				return false, fmt.Errorf("output stream dropped: client too slow")
			}
			idle = 0
			switch fr.Type {
			case proto.MsgOutputRecords:
				var or proto.OutputRecords
				if err := json.Unmarshal(fr.Data, &or); err != nil {
					return false, err
				}
				for _, rec := range or.Records {
					if err := emit(rec); err != nil {
						return false, err
					}
				}
			case proto.MsgOutputEnd:
				var oe proto.OutputEnd
				_ = json.Unmarshal(fr.Data, &oe)
				if oe.Error != "" {
					return true, fmt.Errorf("%s", oe.Error)
				}
				// Without follow the host sends what it has and ends; the
				// placement is only done if it has exited.
				if !follow {
					return pl.state == "exited" || pl.state == "lost", nil
				}
				return true, nil
			}
		}
	}
}
