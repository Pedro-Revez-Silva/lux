package gitws

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Clones of a mirror failed with "trying to write ref ... with nonexistent
// object" beside a fetch's detached gc (ABS-30); mirrorLock explains the
// race. Plain git reproduces it: repack -d plus prune-packed during a clone
// of a large mirror fails 5 runs in 10.

func repo(bare, ref string) Repo {
	return Repo{Tenant: "t", Name: "r", URL: bare, Ref: ref}
}

// pushLoose adds a commit of n new files to the remote.
func pushLoose(t *testing.T, bare, branch string, n int) {
	t.Helper()
	work := filepath.Join(filepath.Dir(bare), "work")
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(work, fmt.Sprintf("%s-%d.txt", branch, i)), []byte(branch+fmt.Sprint(i)), 0o644)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", branch)
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/heads/"+branch)
}

// warm materializes main once, so the mirror exists, and returns its path.
func warm(t *testing.T, m *Manager, bare string) string {
	t.Helper()
	if _, err := m.Materialize(context.Background(), repo(bare, "main"), filepath.Join(t.TempDir(), "warm")); err != nil {
		t.Fatal(err)
	}
	return m.mirrorPath("t", bare)
}

// fetch runs mirror() for ref and releases it at once.
func fetch(t *testing.T, m *Manager, bare, ref string) {
	t.Helper()
	_, release, err := m.mirror(context.Background(), repo(bare, ref), false)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// packEachFetch keeps each fetch as its own pack and makes gc due at two
// packs: the mirror's, repacked into one here, and the next fetch's.
func packEachFetch(t *testing.T, mirror string) {
	t.Helper()
	gitRun(t, mirror, "config", "fetch.unpackLimit", "1")
	gitRun(t, mirror, "config", "gc.autoPackLimit", "1")
	gitRun(t, mirror, "repack", "-q", "-a", "-d")
}

// waitParked returns once git has opened fifo for reading, parking it
// there until the returned file is closed.
func waitParked(t *testing.T, fifo string) *os.File {
	t.Helper()
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestFetchRepacksBeforeReturning(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	// The auto gc a busy mirror reaches, detached as git runs it by default.
	gitRun(t, mirror, "config", "gc.autoDetach", "true")
	gitRun(t, mirror, "config", "maintenance.autoDetach", "true")
	packEachFetch(t, mirror)
	pushLoose(t, bare, "next", 300)
	fetch(t, m, bare, "next")
	// Done when mirror returned: no gc still running, the fetch's pack
	// already folded in.
	if _, err := os.Stat(filepath.Join(mirror, "gc.pid")); err == nil {
		t.Fatal("a gc is still running on the mirror after the fetch returned")
	}
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 1\n") {
		t.Fatalf("the fetch left its repack to a detached gc: %s", out)
	}
}

// A reader (a push or sync, borrowing objects through alternates) holds
// off gc, not other readers and not a fetch; gc runs on the next fetch
// once the mirror is free.
func TestAReaderHoldsOffGCOnly(t *testing.T) {
	bare, sha := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	packEachFetch(t, mirror)
	l := m.lockFor(mirror)
	_, release, err := m.mirror(context.Background(), repo(bare, sha), false)
	if err != nil {
		t.Fatal(err)
	}
	if !l.gc.TryRLock() {
		t.Fatal("a second reader was held up by the first")
	}
	l.gc.RUnlock()
	if l.gc.TryLock() {
		l.gc.Unlock()
		t.Fatal("gc could run while a reader held the mirror")
	}
	// A fetch still runs (it only adds objects), and skips its gc.
	pushLoose(t, bare, "next", 3)
	fetched := make(chan struct{})
	go func() { defer close(fetched); fetch(t, m, bare, "next") }()
	select {
	case <-fetched:
	case <-time.After(30 * time.Second):
		release()
		<-fetched
		t.Fatal("a fetch waited for a reader")
	}
	if out := gitRun(t, mirror, "for-each-ref", "refs/heads/next"); out == "" {
		t.Fatal("the fetch did not run beside a reader")
	}
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 2\n") {
		t.Fatalf("gc ran while a reader held the mirror: %s", out)
	}
	release()
	pushLoose(t, bare, "later", 3)
	fetch(t, m, bare, "later")
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 1\n") {
		t.Fatalf("gc did not run once the mirror was free: %s", out)
	}
}

// A clone holds the mirror until it has copied it. A FIFO in objects/ parks
// the clone mid-copy: opening it for writing returns once the clone opened
// it for reading.
func TestMaterializeHoldsTheMirrorThroughItsClone(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	fifo := filepath.Join(mirror, "objects", "info", "park")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(fifo)
	done := make(chan error, 1)
	go func() {
		_, err := m.Materialize(context.Background(), repo(bare, "main"), filepath.Join(t.TempDir(), "c"))
		done <- err
	}()
	w := waitParked(t, fifo)
	defer w.Close()
	l := m.lockFor(mirror)
	if l.gc.TryLock() {
		l.gc.Unlock()
		t.Fatal("gc could run while a clone was copying the mirror")
	}
	if l.files.TryLock() {
		l.files.Unlock()
		t.Fatal("a fetch could run while a clone was copying the mirror")
	}
	w.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the clone never finished")
	}
	if !l.gc.TryLock() || !l.files.TryLock() {
		t.Fatal("the clone kept the mirror after it finished")
	}
	l.gc.Unlock()
	l.files.Unlock()
}

// A fetch waits for a clone that is copying the mirror (a fetch's temporary
// files come and go in objects/), and runs once the clone is done.
func TestAFetchWaitsForACopy(t *testing.T) {
	bare, sha := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	_, release, err := m.mirror(context.Background(), repo(bare, sha), true)
	if err != nil {
		t.Fatal(err)
	}
	pushLoose(t, bare, "next", 3)
	fetched := make(chan struct{})
	go func() { defer close(fetched); fetch(t, m, bare, "next") }()
	// Can pass for a slow fetch on a loaded host, never fail for a right one.
	select {
	case <-fetched:
		t.Fatal("a fetch ran while a clone was copying the mirror")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case <-fetched:
	case <-time.After(30 * time.Second):
		t.Fatal("the fetch never ran after the copy")
	}
	if out := gitRun(t, mirror, "for-each-ref", "refs/heads/next"); out == "" {
		t.Fatal("the fetch did not land")
	}
}

// Callers that queued behind a fetch use it: a burst of starts for one
// repository fetches once, not once per Run. A fetch that started before a
// caller asked is not enough for it.
func TestQueuedFetchesUseTheOneBeforeThem(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	l := m.lockFor(mirror)
	r := repo(bare, "main")
	asked := time.Now()
	l.fetchedAt = asked.Add(time.Millisecond) // a fetch started after we asked
	pushLoose(t, bare, "skipped", 1)
	if err := m.update(context.Background(), r, mirror, l, asked); err != nil {
		t.Fatal(err)
	}
	if out := gitRun(t, mirror, "for-each-ref", "refs/heads/skipped"); out != "" {
		t.Fatal("fetched again although a fetch had started since this caller asked")
	}
	l.fetchedAt = asked.Add(-time.Millisecond) // the last fetch began before we asked
	if err := m.update(context.Background(), r, mirror, l, asked); err != nil {
		t.Fatal(err)
	}
	if out := gitRun(t, mirror, "for-each-ref", "refs/heads/skipped"); out == "" {
		t.Fatal("did not fetch although the last fetch began before this caller asked")
	}
}

// parkGit puts a git wrapper first on PATH that, for a command line
// containing word, opens the returned FIFO for reading (so it blocks until
// the test opens it for writing) before running the real git.
func parkGit(t *testing.T, word string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "park")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = \"" + word + "\" ] && cat " + fifo + " >/dev/null && break; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return fifo
}

// fetchedAt is when the fetch started, not when it ended: a caller that
// asked during a fetch is not served by it, since its ref may have moved
// after the fetch read the remote's.
func TestFetchedAtIsWhenTheFetchStarted(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	fifo := parkGit(t, "--prune")
	done := make(chan struct{})
	go func() { defer close(done); fetch(t, m, bare, "main") }()
	w := waitParked(t, fifo)
	mid := time.Now()
	w.Close()
	<-done
	if l := m.lockFor(mirror); !l.fetchedAt.Before(mid) {
		t.Fatal("fetchedAt is after a moment inside the fetch")
	}
}

// Push and SyncBundle hold gc off while their scratch repository borrows
// the mirror's objects through alternates: each is parked on its last git
// command that reads them.
func TestPushAndSyncHoldOffGC(t *testing.T) {
	for _, c := range []struct{ name, word string }{{"sync", "create"}, {"push", "--porcelain"}} {
		t.Run(c.name, func(t *testing.T) {
			bare, _ := remote(t)
			m := New(t.TempDir())
			mirror := warm(t, m, bare)
			var bundle string
			if c.name == "push" {
				dir := filepath.Join(t.TempDir(), "r")
				if _, err := m.Materialize(context.Background(), repo(bare, "main"), dir); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
				gitRun(t, dir, "commit", "-qam", "two")
				bundle = filepath.Join(t.TempDir(), "push.bundle")
				gitRun(t, dir, "bundle", "create", "-q", bundle, "HEAD")
			}
			fifo := parkGit(t, c.word)
			done := make(chan error, 1)
			go func() {
				r := repo(bare, "main")
				if c.name == "sync" {
					_, err := m.SyncBundle(context.Background(), r, "", filepath.Join(t.TempDir(), "s.bundle"))
					done <- err
					return
				}
				res := m.Push(context.Background(), r, bundle, "lux/x", "")
				if res.Status != "pushed" {
					done <- fmt.Errorf("push: %+v", res)
					return
				}
				done <- nil
			}()
			w := waitParked(t, fifo)
			l := m.lockFor(mirror)
			held := !l.gc.TryLock()
			if !held {
				l.gc.Unlock()
			}
			w.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !held {
				t.Fatalf("gc could run while a %s's scratch borrowed the mirror", c.name)
			}
		})
	}
}

// The symptom end to end: clones of a mirror while fetches into it each
// leave loose objects and make a repack due. Probabilistic: main fails a
// few of 60 (a detached gc); a fetch beside a copy (its temporary files)
// fails it only rarely, which TestAFetchWaitsForACopy pins instead.
func TestClonesSurviveConcurrentFetchAndRepack(t *testing.T) {
	if testing.Short() {
		t.Skip("about a minute")
	}
	bare, _ := remote(t)
	work := filepath.Join(filepath.Dir(bare), "work")
	for c := 0; c < 20; c++ {
		for i := 0; i < 200; i++ {
			os.WriteFile(filepath.Join(work, fmt.Sprintf("f%d-%d.txt", c, i)), []byte(fmt.Sprint(c, i)), 0o644)
		}
		gitRun(t, work, "add", "-A")
		gitRun(t, work, "commit", "-qm", "bulk")
	}
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/heads/main")
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	gitRun(t, mirror, "config", "fetch.unpackLimit", "100000")
	gitRun(t, mirror, "config", "gc.auto", "1")
	gitRun(t, mirror, "config", "gc.autoPackLimit", "1")
	stop := make(chan struct{})
	fetched := make(chan struct{})
	go func() {
		defer close(fetched)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			pushLoose(t, bare, fmt.Sprintf("b%d", n), 300)
			if _, release, err := m.mirror(context.Background(), repo(bare, "main"), false); err == nil {
				release()
			}
		}
	}()
	root := t.TempDir()
	var failed []error
	for i := 0; i < 60; i++ {
		if _, err := m.Materialize(context.Background(), repo(bare, "main"), filepath.Join(root, fmt.Sprint(i))); err != nil {
			failed = append(failed, err)
		}
	}
	close(stop)
	<-fetched
	if len(failed) > 0 {
		t.Fatalf("%d of 60 clones failed; first: %v", len(failed), failed[0])
	}
}
