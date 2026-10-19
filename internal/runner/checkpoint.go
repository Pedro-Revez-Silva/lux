package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

func (p *placement) checkpointLimit() int64 {
	if p.assign != nil && p.assign.CheckpointBytes > 0 {
		return p.assign.CheckpointBytes
	}
	return proto.DefaultCheckpointBytes
}

func (p *placement) checkpointRunning(ctx context.Context) {
	if p.assign == nil || p.assign.Spec.Git == nil {
		return
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return
	}
	rtRoot, err := os.OpenRoot(rt)
	if err != nil {
		return
	}
	defer rtRoot.Close()
	dir := "checkpoint-live-" + ids.New("stage")
	if rtRoot.Mkdir(dir, 0o700) != nil {
		return
	}
	// Collected bundles are copied into the blob store; the stage, written
	// by the workload user, is removed through the root so no link it
	// planted is followed.
	defer func() { _ = rtRoot.RemoveAll(dir) }()
	if rtRoot.Chown(dir, p.user.UID, p.user.GID) != nil {
		return
	}
	a := proto.CheckpointArgs{Dir: proto.ShimRunDir + "/" + dir, MaxBytes: p.checkpointLimit()}
	p.mu.Lock()
	for _, repo := range p.assign.Spec.Git.Repositories {
		a.Repos = append(a.Repos, proto.CheckpointRepo{Name: repo.Name, Path: repo.Path, Previous: p.assign.CheckpointRefs[repo.Name]})
	}
	p.mu.Unlock()
	out, err := p.r.execShim(ctx, p.runID, fmt.Sprintf("%d:%d", p.user.UID, p.user.GID), "checkpoint", a)
	if err != nil {
		p.event(ctx, proto.EvWarning, map[string]any{"message": "git checkpoint: " + err.Error()})
		return
	}
	// The manifest is replaced, never opened through a link planted by the
	// workload.
	root, err := rtRoot.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.Remove("results.json")
	f, err := root.OpenFile("results.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, err = f.Write(out)
	f.Close()
	if err != nil {
		return
	}
	p.collectCheckpointDir(ctx, dir)
}

func (p *placement) collectCheckpoints(ctx context.Context) {
	p.collectCheckpointDir(ctx, fmt.Sprintf("checkpoints-%d", p.epoch))
}

// openStaged opens a file of a workload-writable staging dir for reading:
// never through a link, never blocking on a FIFO, and only a regular file.
func openStaged(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if err != nil {
		return nil, err
	}
	return regularFile(f, name)
}

// openRegular opens a file at a fixed path the workload can replace (on
// the runtime volume) for reading: never blocking on a FIFO, and only a
// regular file.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNonBlock, 0)
	if err != nil {
		return nil, err
	}
	return regularFile(f, path)
}

func regularFile(f *os.File, name string) (*os.File, error) {
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (p *placement) collectCheckpointDir(ctx context.Context, dir string) {
	if p.assign == nil || p.assign.Spec.Git == nil || len(p.assign.Spec.Git.Repositories) == 0 {
		return
	}
	warn := func(err error) {
		p.event(ctx, proto.EvWarning, map[string]any{"message": "git checkpoint: " + err.Error()})
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		warn(err)
		return
	}
	root, err := os.OpenRoot(rt)
	if err != nil {
		warn(err)
		return
	}
	defer root.Close()
	f, err := openStaged(root, dir+"/results.json")
	if err != nil {
		warn(fmt.Errorf("no completed checkpoint stage: %w", err))
		return
	}
	var results []proto.StagedCheckpoint
	err = json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&results)
	f.Close()
	if err != nil {
		warn(err)
		return
	}
	for i, repo := range p.assign.Spec.Git.Repositories {
		if i >= len(results) || results[i].Repo != repo.Name {
			warn(fmt.Errorf("%s: missing stage result", repo.Name))
			continue
		}
		res := results[i]
		if res.Unchanged {
			// Its latest checkpoint stands; a waiting checkpoint request
			// counts the repository as done.
			p.event(ctx, "git.checkpoint", map[string]any{"repo": repo.Name, "unchanged": true})
			continue
		}
		if res.Error != "" {
			warn(fmt.Errorf("%s: %s", repo.Name, res.Error))
			continue
		}
		if res.File != strconv.Itoa(i)+".bundle" || res.Head == "" || res.Refs["HEAD"] != res.Head {
			warn(fmt.Errorf("%s: invalid stage result", repo.Name))
			continue
		}
		src, err := openStaged(root, dir+"/"+res.File)
		if err != nil {
			warn(fmt.Errorf("%s: %w", repo.Name, err))
			continue
		}
		fi, err := src.Stat()
		limit := p.checkpointLimit()
		if err != nil || fi.Size() != res.Size || res.Size > limit {
			src.Close()
			warn(fmt.Errorf("%s: invalid bundle size or type", repo.Name))
			continue
		}
		id := ids.New(ids.Blob)
		size, sum, err := p.r.writeBlob(id, func(w io.Writer) error {
			copied, err := io.Copy(w, io.LimitReader(src, limit+1))
			if err == nil && copied != res.Size {
				return fmt.Errorf("bundle changed while collected")
			}
			return err
		})
		src.Close()
		if err != nil {
			warn(err)
			continue
		}
		ready := &proto.CheckpointReady{Blob: proto.BlobInfo{BlobID: id, Size: size, SHA256: sum}, StagedCheckpoint: res}
		rec := &snapshotRecord{RunID: p.runID, Epoch: p.epoch, Created: time.Now().UnixMilli(), Checkpoint: ready, Uploads: []pendingUpload{{BlobID: id, Path: p.r.blobPath(id), Size: size}}}
		if err := p.r.saveSnapshotRecord(id, rec); err != nil {
			warn(err)
			continue
		}
		ack, err := p.reportAck(ctx, proto.MsgCheckpointReady, ready)
		if err != nil {
			warn(err)
			continue
		}
		if ack.Refused {
			removeSnapshotFiles(p.r, id, rec)
			continue
		}
		p.r.updateRecord(id, func(r *snapshotRecord) { r.Reported = true })
		names := make([]string, 0, len(res.Refs))
		for ref := range res.Refs {
			if ref != "HEAD" {
				names = append(names, ref)
			}
		}
		slices.Sort(names)
		var lines []string
		for _, ref := range names {
			lines = append(lines, res.Refs[ref]+" "+ref)
		}
		lines = append(lines, res.Head+" HEAD")
		p.mu.Lock()
		if p.assign.CheckpointRefs == nil {
			p.assign.CheckpointRefs = map[string]string{}
		}
		p.assign.CheckpointRefs[repo.Name] = strings.Join(lines, "\n")
		p.mu.Unlock()
		p.r.uploads.kick()
	}
}
