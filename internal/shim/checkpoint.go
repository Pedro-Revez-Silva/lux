package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func Checkpoint(args []string) int {
	var a proto.CheckpointArgs
	if len(args) != 1 || json.Unmarshal([]byte(args[0]), &a) != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	results := make([]proto.StagedCheckpoint, 0, len(a.Repos))
	for i, repo := range a.Repos {
		results = append(results, checkpointRepo(ctx, a, repo, i))
	}
	if json.NewEncoder(os.Stdout).Encode(results) != nil {
		return 1
	}
	return 0
}

func checkpointRepo(ctx context.Context, a proto.CheckpointArgs, repo proto.CheckpointRepo, index int) (res proto.StagedCheckpoint) {
	res.Repo = repo.Name
	name := strconv.Itoa(index) + ".bundle"
	file := filepath.Join(a.Dir, name)
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(file)
		}
	}()
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", proto.BundleScript, "sh", "checkpoint", file, repo.Previous}, proto.OperationStates...)...)
	cmd.Dir = repo.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		res.Error = fmt.Sprintf("git bundle: %v: %.1000s", err, out)
		return
	}
	switch got := strings.TrimSpace(string(out)); {
	case got == "lux-unchanged":
		res.Unchanged = true
		return
	case got != "":
		res.Error = "git bundle: " + got
		return
	}
	fi, err := os.Lstat(file)
	if err != nil {
		res.Error = err.Error()
		return
	}
	limit := a.MaxBytes
	if limit <= 0 {
		limit = proto.DefaultCheckpointBytes
	}
	if !fi.Mode().IsRegular() || fi.Size() > limit {
		res.Error = fmt.Sprintf("checkpoint bundle is not a regular file within the %d-byte limit", limit)
		return
	}
	heads, err := gitIn(ctx, repo.Path, "bundle", "list-heads", file)
	if err != nil {
		res.Error = err.Error()
		return
	}
	res.Refs = map[string]string{}
	for _, line := range strings.Split(heads, "\n") {
		sha, ref, ok := strings.Cut(line, " ")
		if !ok || (ref != "HEAD" && !strings.HasPrefix(ref, "refs/heads/")) {
			res.Error = "invalid bundle ref list"
			return
		}
		res.Refs[ref] = sha
	}
	res.Head = res.Refs["HEAD"]
	if res.Head == "" {
		res.Error = "bundle has no HEAD"
		return
	}
	res.Branch, _ = gitIn(ctx, repo.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	res.Size, res.File = fi.Size(), name
	keep = true
	return
}

// stageCheckpoints runs after the workload and its stop hook have ended.
// Git reads checkout-controlled configuration only in the unprivileged child.
func (s *Shim) stageCheckpoints() {
	if s.cfg.Checkpoint == nil || len(s.cfg.Checkpoint.Repos) == 0 || s.user == nil {
		return
	}
	a := *s.cfg.Checkpoint
	fail := func(err error) {
		s.out.Event(proto.EvWarning, map[string]any{"message": "git checkpoint: " + err.Error()})
	}
	// A spot reclamation's kill deadline (seconds) is shorter than staging
	// may take (up to 65s): the stop's last moments are the exit record's.
	s.mu.Lock()
	reclaimed := s.reclaimed
	s.mu.Unlock()
	if reclaimed {
		fail(fmt.Errorf("skipped on a spot reclamation"))
		return
	}
	if err := os.Mkdir(a.Dir, 0o700); err != nil {
		fail(err)
		return
	}
	if err := os.Chown(a.Dir, int(s.user.uid), int(s.user.gid)); err != nil {
		fail(err)
		return
	}
	cmd := s.command([]string{proto.ShimBinary, "checkpoint", string(proto.Marshal(a))}, s.env)
	cmd.Dir = "/"
	cmd.SysProcAttr.UseCgroupFD = false
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	exited, err := s.startTracked(cmd.Start, &cmd.Process)
	if err != nil {
		fail(err)
		return
	}
	select {
	case ws := <-exited:
		_ = cmd.Wait()
		if ws.ExitStatus() != 0 {
			fail(fmt.Errorf("exit %d: %.1000s", ws.ExitStatus(), out.String()))
			return
		}
	case <-time.After(65 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		_ = cmd.Wait()
		fail(fmt.Errorf("staging timed out"))
		return
	}
	var results []proto.StagedCheckpoint
	if err := json.Unmarshal([]byte(out.String()), &results); err != nil {
		fail(err)
		return
	}
	for _, res := range results {
		if res.Error != "" {
			fail(fmt.Errorf("%s: %s", res.Repo, res.Error))
		}
	}
	b := proto.Marshal(results)
	// The manifest is written by the shim, never opened through a workload link.
	root, err := os.OpenRoot(a.Dir)
	if err != nil {
		fail(err)
		return
	}
	defer root.Close()
	if err := root.Remove("results.json"); err != nil && !os.IsNotExist(err) {
		fail(err)
		return
	}
	f, err := root.OpenFile("results.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fail(err)
		return
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		fail(err)
	} else if closeErr != nil {
		fail(closeErr)
	}
}
