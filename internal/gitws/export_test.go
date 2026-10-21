package gitws

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Export writes the tree at a commit, without .git, from the tenant's own
// mirror; a commit only a non-branch ref has (a pull request's head) is
// fetched by its sha.
func TestExportWritesTheCommitsTree(t *testing.T) {
	bare, first := remote(t)
	work := filepath.Join(filepath.Dir(bare), "work")
	os.MkdirAll(filepath.Join(work, "sub"), 0o755)
	os.WriteFile(filepath.Join(work, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("a.txt", filepath.Join(work, "link"))
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", "two")
	second := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/heads/main")
	// A commit on no branch, as a pull request's head is.
	gitRun(t, work, "checkout", "-q", "--detach")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("pr\n"), 0o644)
	gitRun(t, work, "commit", "-qam", "pr")
	pr := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/pull/1/head")
	gitRun(t, bare, "config", "uploadpack.allowReachableSHA1InWant", "true")

	m := New(t.TempDir())
	export := func(ref string) (string, error) {
		dst := t.TempDir()
		return dst, m.Export(context.Background(), Repo{Tenant: "t", Name: "ctx", URL: bare, Ref: ref}, dst, 1<<20)
	}
	dst, err := export(second)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "one\n" {
		t.Fatalf("a.txt %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dst, "sub", "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh: %v %v", fi, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "a.txt" {
		t.Fatalf("link: %q %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		t.Fatal("the context has .git")
	}
	if dst, err = export(first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "sub")); err == nil {
		t.Fatal("the first commit's tree has sub/")
	}
	if dst, err = export(pr); err != nil {
		t.Fatalf("a pull request's commit: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "pr\n" {
		t.Fatalf("pr a.txt %q", b)
	}
	for _, ref := range []string{"main", second[:12], "--output=/tmp/x", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"} {
		if _, err := export(ref); err == nil {
			t.Errorf("%q exported", ref)
		}
	}
	// Bounded by the limit.
	if err := m.Export(context.Background(), Repo{Tenant: "t", Name: "ctx", URL: bare, Ref: second}, t.TempDir(), 3); !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	// Only the tenant's own mirror.
	if len(m.known) != 1 {
		t.Fatalf("mirrors %v", m.known)
	}
}

// A hostile archive cannot write outside the context: not by .., not by
// an absolute name, not through a symlink it made.
func TestUntarStaysInside(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]tar.Header{
		"dotdot":   {{Name: "../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		"absolute": {{Name: filepath.Join(outside, "escape"), Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		"symlink dir": {
			{Name: "d", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "d/escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		},
		"symlink file": {
			{Name: "f", Typeflag: tar.TypeSymlink, Linkname: filepath.Join(outside, "escape")},
			{Name: "f", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range entries {
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if h.Size > 0 {
				tw.Write([]byte("x"))
			}
		}
		tw.Close()
		dst := t.TempDir()
		root, err := os.OpenRoot(dst)
		if err != nil {
			t.Fatal(err)
		}
		err = untar(root, &buf, 1<<20)
		root.Close()
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Lstat(filepath.Join(outside, "escape")); err == nil {
			t.Fatalf("%s: wrote outside the context", name)
		}
	}
}
