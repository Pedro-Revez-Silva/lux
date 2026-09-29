package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/proto"
)

// Blobs live under <data>/snapshots/<blob id>.zst until uploaded and no
// longer needed locally.

func (r *Runner) blobPath(blobID string) string {
	return filepath.Join(r.cfg.DataDir, "snapshots", blobID+".zst")
}

func (r *Runner) recordPath(snapID string) string {
	return filepath.Join(r.cfg.DataDir, "snapshots", snapID+".json")
}

// writeBlob compresses what produce writes into a new blob file. Returns
// the compressed size and its sha256.
func (r *Runner) writeBlob(blobID string, produce func(io.Writer) error) (int64, string, error) {
	return r.writeBlobLevel(blobID, zstd.SpeedDefault, produce)
}

func (r *Runner) writeBlobLevel(blobID string, level zstd.EncoderLevel, produce func(io.Writer) error) (int64, string, error) {
	path := r.blobPath(blobID)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	zw, err := zstd.NewWriter(cw, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
	if err != nil {
		f.Close()
		return 0, "", err
	}
	if err := produce(zw); err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return 0, "", err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, "", err
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		return 0, "", err
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (r *Runner) saveSnapshotRecord(snapID string, rec *snapshotRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.recordPath(snapID), b, 0o600)
}

func (r *Runner) snapshotRecords() map[string]*snapshotRecord {
	out := map[string]*snapshotRecord{}
	entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "snapshots"))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.cfg.DataDir, "snapshots", name))
		if err != nil {
			continue
		}
		var rec snapshotRecord
		if json.Unmarshal(b, &rec) == nil {
			out[strings.TrimSuffix(name, ".json")] = &rec
		}
	}
	return out
}

// uploader uploads blobs in the background, oldest first, retrying until
// luxd has them. Runs across restarts: pending uploads are on disk.
type uploader struct {
	r  *Runner
	kc chan struct{}
}

func newUploader(r *Runner) *uploader { return &uploader{r: r, kc: make(chan struct{}, 1)} }

func (u *uploader) kick() {
	select {
	case u.kc <- struct{}{}:
	default:
	}
}

func (u *uploader) loop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		u.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-u.kc:
		}
	}
}

func (u *uploader) pass(ctx context.Context) {
	for snapID, rec := range u.r.snapshotRecords() {
		// Only once luxd has the report that lists them: before, it does
		// not know the blobs (and answers 404). A record that will never be
		// reported (its placement was fenced off, or a later placement of
		// the Run on this host replaced it) has nothing to upload: its
		// files go once its Run's local copy is discarded.
		if !rec.Reported {
			if rec.Discard && u.r.unreportable(rec) {
				removeSnapshotFiles(u.r, snapID, rec)
			}
			continue
		}
		if u.r.isStaleRun(rec.RunID, rec.Epoch) {
			continue
		}
		done := true
		for _, up := range rec.Uploads {
			if up.Done {
				continue
			}
			err := u.upload(ctx, up)
			if errors.Is(err, os.ErrNotExist) && rec.Diffs {
				// A patch owed to luxd is gone: its diff has failed,
				// which luxd must hear; nothing more of it is uploaded.
				u.r.log.Warn("snapshot diff: a patch to upload is missing; its diff has failed", "record", snapID, "blob", up.BlobID)
				u.r.diffsLost(ctx, snapID, rec, "the patch file of blob "+up.BlobID+" is missing on its host")
				done = false
				break
			}
			if errors.Is(err, os.ErrNotExist) {
				err = nil // its file is gone: nothing to upload
			}
			var he *httpError
			if err != nil && errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusGone) {
				err = nil // luxd deleted it (retention): nothing to do
			}
			if err != nil {
				if ctx.Err() == nil {
					u.r.log.Warn("upload failed; will retry", "blob", up.BlobID, "err", err)
				}
				done = false
				break
			}
			// Durable per blob: a restart does not upload it again.
			u.r.updateRecord(snapID, func(cur *snapshotRecord) {
				for i := range cur.Uploads {
					if cur.Uploads[i].BlobID == up.BlobID {
						cur.Uploads[i].Done = true
					}
				}
			})
		}
		// A discarded Run's last uploads are done: nothing left to keep.
		if done && rec.Discard {
			removeSnapshotFiles(u.r, snapID, rec)
		}
	}
}

// markReported records that luxd has a snapshot's report; and so an
// earlier record of the same placement (same epoch: a restarted runner
// finished it again), whose report went through before the runner could
// mark it. Other placements' records are theirs alone.
func (r *Runner) markReported(snapID string) {
	var runID string
	var epoch int
	r.updateRecord(snapID, func(rec *snapshotRecord) {
		rec.Reported = true
		runID, epoch = rec.RunID, rec.Epoch
	})
	for id, rec := range r.snapshotRecords() {
		if id != snapID && !rec.Reported && !rec.Diffs && rec.RunID == runID && rec.Epoch == epoch {
			r.updateRecord(id, func(rec *snapshotRecord) { rec.Reported = true })
		}
	}
}

// unreportable: a record whose report will never be sent: its placement
// was fenced off, or the Run has a later placement on this host.
func (r *Runner) unreportable(rec *snapshotRecord) bool {
	st, err := readRunState(r.runDir(rec.RunID))
	if err != nil {
		return true // the Run's local state is gone
	}
	return st.Epoch > rec.Epoch || (st.Stale && st.Epoch == rec.Epoch)
}

// updateRecord changes a snapshot record on disk, from its current
// contents: each writer changes only its own fields.
func (r *Runner) updateRecord(snapID string, fn func(*snapshotRecord)) {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	b, err := os.ReadFile(r.recordPath(snapID))
	if err != nil {
		return
	}
	var rec snapshotRecord
	if json.Unmarshal(b, &rec) == nil {
		fn(&rec)
		_ = r.saveSnapshotRecord(snapID, &rec)
	}
}

// upload uploads one blob; os.ErrNotExist when its file is gone.
func (u *uploader) upload(ctx context.Context, up pendingUpload) error {
	f, err := os.Open(up.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return u.r.api.upload(ctx, up.BlobID, f, fi.Size())
}

func (r *Runner) isStaleRun(runID string, epoch int) bool {
	st, err := readRunState(r.runDir(runID))
	return err == nil && st.Stale && st.Epoch == epoch
}

// discard deletes the local copy of a Run's state: luxd says it resumed
// elsewhere from an uploaded snapshot.
func (r *Runner) discard(ctx context.Context, runID string, beforeEpoch int) {
	r.mu.Lock()
	p := r.placements[runID]
	r.mu.Unlock()
	if p != nil && p.epoch >= beforeEpoch {
		return
	}
	// Not while a snapshot diff's helper may still have the volumes
	// mounted: once it is confirmed gone, or never (the copy stays). Nor
	// before its patches are in their record, which the scan below must
	// see to keep them until uploaded.
	if p != nil {
		err := r.volumesFree(ctx, runID, p.supersede(errDiscarded), true)
		if err == nil {
			err = awaitClosed(ctx, p.diffHandoff(), fmt.Sprintf("the snapshot diff's patches were not recorded in %s", volumesWait))
		}
		if err != nil {
			r.log.Error("discard of the local copy failed; it stays", "run", runID, "err", err)
			return
		}
	}
	r.mu.Lock()
	if cur := r.placements[runID]; cur != p {
		r.mu.Unlock()
		return // a new placement meanwhile: its volumes now
	}
	delete(r.placements, runID)
	r.mu.Unlock()
	r.removeRunLocal(ctx, runID)
}

var errDiscarded = errors.New("the Run's local copy was discarded")

func (r *Runner) removeRunLocal(ctx context.Context, runID string) {
	_ = r.pm.Remove(ctx, containerName(runID))
	vols, _ := r.pm.VolumeList(ctx, LabelRun+"="+runID)
	for _, v := range vols {
		_ = r.pm.VolumeRemove(ctx, v)
		r.forgetMountpoint(v)
	}
	r.egress.Remove(bridgeName(runID))
	_ = r.pm.NetworkRemove(ctx, networkName(runID))
	for snapID, rec := range r.snapshotRecords() {
		if rec.RunID != runID {
			continue
		}
		pending := false
		for _, up := range rec.Uploads {
			if !up.Done {
				pending = true
			}
		}
		if pending {
			// Still owed to luxd: the uploader deletes it once uploaded.
			r.updateRecord(snapID, func(cur *snapshotRecord) { cur.Discard = true })
			continue
		}
		removeSnapshotFiles(r, snapID, rec)
	}
	os.RemoveAll(r.runDir(runID))
	r.log.Info("discarded local copy", "run", runID)
}

func removeSnapshotFiles(r *Runner, snapID string, rec *snapshotRecord) {
	for _, up := range rec.Uploads {
		os.Remove(up.Path)
	}
	os.Remove(r.recordPath(snapID))
}

// gcLoop removes local state for Runs that ended here longer ago than the
// host TTL, once everything is uploaded.
func (r *Runner) gcLoop(ctx context.Context) {
	// Often enough that a copy outlives its TTL by at most half again.
	t := time.NewTicker(min(r.cfg.HostTTL/2, time.Minute))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.gcImages(ctx, r.cfg.HostTTL)
		entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "runs"))
		for _, e := range entries {
			st, err := readRunState(r.runDir(e.Name()))
			if err != nil || st.LastExitAt == 0 {
				continue
			}
			if st.Phase != "reported" && !st.Stale {
				continue
			}
			if time.Since(time.UnixMilli(st.LastExitAt)) < r.cfg.HostTTL {
				continue
			}
			r.mu.Lock()
			p := r.placements[e.Name()]
			busy := p != nil && p.liveState() != ""
			r.mu.Unlock()
			if !busy {
				r.discard(ctx, e.Name(), math.MaxInt)
			}
		}
	}
}

// ---- restart: re-adopt containers ----------------------------------------

// readopt finds this runner's containers after a restart. Running ones are
// supervised again; ones that exited while the runner was down are
// finished (snapshot and report) now.
func (r *Runner) readopt(ctx context.Context) {
	entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "runs"))
	for _, e := range entries {
		st, err := readRunState(r.runDir(e.Name()))
		if err != nil || st.Stale {
			continue
		}
		p := &placement{
			r: r, runID: st.RunID, tenantID: st.TenantID, epoch: st.Epoch, dir: r.runDir(st.RunID),
			state: st, done: make(chan struct{}), stopWhy: st.StopReason,
		}
		// No assignment after a restart: the spec it had, without secrets.
		if st.Spec != nil {
			p.assign = &proto.Assign{RunID: st.RunID, TenantID: st.TenantID, Epoch: st.Epoch, Spec: *st.Spec}
		}
		switch st.Phase {
		case "reported":
			p.phase = "done"
			close(p.done)
			// Diffs owed for its last snapshot, which luxd has, start once
			// luxd's welcome says it takes them (startOwedDiffs).
		case "exited":
			p.phase = "exited"
			r.log.Info("re-adopting exited placement", "run", st.RunID, "epoch", st.Epoch)
			go func() { defer close(p.done); p.finish(context.WithoutCancel(ctx), st.Exit) }()
		case "started":
			cs, _ := r.pm.Inspect(ctx, containerName(st.RunID))
			if !cs.Exists {
				p.phase = "done"
				close(p.done)
				break
			}
			p.cgroup = cs.CgroupPath
			if cs.Running {
				// Before anything else: a running container must not spend a
				// moment without its egress rules.
				if err := p.applyEgress(ctx, p.state.Egress); err != nil {
					r.log.Error("re-adopt: egress; killing the container", "run", st.RunID, "err", err)
					_ = r.pm.Kill(ctx, containerName(st.RunID), "KILL")
				}
				p.phase = "running"
				if p.stopWhy != "" {
					p.phase = "stopping"
				}
				r.log.Info("re-adopting running placement", "run", st.RunID, "epoch", st.Epoch)
				go func() {
					defer close(p.done)
					if err := p.dialShim(ctx); err != nil {
						r.log.Warn("re-adopt: shim", "run", st.RunID, "err", err)
					}
					p.supervise(context.WithoutCancel(ctx))
				}()
			} else {
				p.phase = "exited"
				go func() { defer close(p.done); p.supervise(context.WithoutCancel(ctx)) }()
			}
		default:
			// Assigned but never started: luxd redelivers the assignment
			// (unacked) or has given up on it.
			continue
		}
		r.placements[st.RunID] = p
	}
}

// ---- output streaming -------------------------------------------------------

// streamOutput sends a placement's records to luxd for a subscriber.
func (r *Runner) streamOutput(ctx context.Context, runID string, epoch int, s proto.OutputSubscribe) error {
	done := func() bool {
		r.mu.Lock()
		p := r.placements[runID]
		r.mu.Unlock()
		if p == nil || p.epoch != epoch {
			return true
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.phase == "exited" || p.phase == "done"
	}
	// The placement may not have created its runtime volume yet.
	path, err := r.outputFile(ctx, runID, epoch)
	for err != nil && s.Follow && !done() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		path, err = r.outputFile(ctx, runID, epoch)
	}
	if err != nil {
		if done() {
			return nil // nothing was ever written
		}
		return err
	}
	var batch []proto.Record
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := r.conn.Send(ctx, proto.Frame{Type: proto.MsgOutputRecords, RunID: runID, Epoch: epoch,
			Data: proto.Marshal(proto.OutputRecords{SubID: s.SubID, Records: batch})})
		batch = batch[:0]
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	recs := make(chan proto.Record, 256)
	errc := make(chan error, 1)
	go func() {
		errc <- tailRecords(ctx, path, s.Since, s.Follow, done, func(rec proto.Record) error {
			select {
			case recs <- rec:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(recs)
	}()
	for {
		select {
		case rec, ok := <-recs:
			if !ok {
				if err := flush(); err != nil {
					return err
				}
				return <-errc
			}
			batch = append(batch, rec)
			if len(batch) >= 200 {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

func (r *Runner) outputFile(ctx context.Context, runID string, epoch int) (string, error) {
	rt, err := r.mountpoint(ctx, runtimeVolume(runID))
	if err != nil {
		return "", err
	}
	return filepath.Join(rt, proto.OutputFile(epoch)), nil
}
