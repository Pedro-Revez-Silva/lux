package gitws

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// A hostile tree cannot write outside the context: not by .., not by an
// absolute name, not through a symlink it made.
func TestTreeStaysInside(t *testing.T) {
	outside := t.TempDir()
	blobs := map[string]string{"x": "x", "dir": outside, "file": filepath.Join(outside, "escape")}
	read := func(oid string, fn func(io.Reader, int64) error) error {
		return fn(strings.NewReader(blobs[oid]), int64(len(blobs[oid])))
	}
	for name, entries := range map[string][]treeEntry{
		"dotdot":       {{"100644", "x", "../escape"}},
		"absolute":     {{"100644", "x", filepath.Join(outside, "escape")}},
		"unclean":      {{"100644", "x", "a/../../escape"}},
		"symlink dir":  {{"120000", "dir", "d"}, {"100644", "x", "d/escape"}},
		"symlink file": {{"120000", "file", "f"}, {"100644", "x", "f"}},
	} {
		dst := t.TempDir()
		root, err := os.OpenRoot(dst)
		if err != nil {
			t.Fatal(err)
		}
		err = writeTree(root, entries, read, 1<<20)
		root.Close()
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Lstat(filepath.Join(outside, "escape")); err == nil {
			t.Fatalf("%s: wrote outside the context", name)
		}
	}
}

// The export is the commit's tree byte for byte: its .gitattributes do
// not apply (export-subst would expand $Format:...$ with whatever tags and
// branches the repository has now; export-ignore would drop committed
// files), so a tag pushed later changes nothing.
func TestExportIsTheLiteralTree(t *testing.T) {
	bare, _ := remote(t)
	work := filepath.Join(filepath.Dir(bare), "work")
	files := map[string]string{
		".gitattributes": "decoration.txt export-subst\nneeded.conf export-ignore\n* text=auto eol=crlf\n",
		"decoration.txt": "$Format:%D %H$\n",
		"needed.conf":    "required by COPY --from=repo\n",
	}
	for name, text := range files {
		os.WriteFile(filepath.Join(work, name), []byte(text), 0o644)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", "pinned inputs")
	sha := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/heads/main")
	export := func() string {
		dst := t.TempDir()
		if err := New(t.TempDir()).Export(context.Background(), Repo{Tenant: "t", Name: "repo", URL: bare, Ref: sha}, dst, 1<<20); err != nil {
			t.Fatal(err)
		}
		for name, want := range files {
			if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != want {
				t.Fatalf("%s: %q %v, want %q", name, b, err, want)
			}
		}
		return dst
	}
	export()
	gitRun(t, work, "-c", "tag.gpgSign=false", "tag", "UNPINNED-DECORATION", sha)
	gitRun(t, work, "push", "-q", bare, "refs/tags/UNPINNED-DECORATION")
	export()
}

// A remote that advertises refs/replace/<sha> can't change what a pinned
// commit exports: the mirror gets the replacement ref (git clone --mirror
// takes every ref), but Export reads objects as they are, whatever the
// runner's environment says.
func TestExportIgnoresReplacementRefs(t *testing.T) {
	bare, commit := remote(t)
	work := filepath.Join(filepath.Dir(bare), "work")
	gitRun(t, work, "checkout", "-q", "--orphan", "replacement")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("replacement payload\n"), 0o644)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", "replacement")
	gitRun(t, work, "replace", commit, gitRun(t, work, "rev-parse", "HEAD"))
	gitRun(t, work, "push", "-q", bare, "refs/replace/*:refs/replace/*")
	t.Setenv("GIT_REPLACE_REF_BASE", "refs/replace/")

	m := New(t.TempDir())
	dst := t.TempDir()
	if err := m.Export(context.Background(), Repo{Tenant: "t", Name: "ctx", URL: bare, Ref: commit}, dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "one\n" {
		t.Fatalf("exported %q, not the commit's own tree", b)
	}
	// The hazard is real: the mirror has the replacement, and git with
	// replacement objects on would read it.
	mirror := m.mirrorPath("t", bare)
	if refs := gitRun(t, mirror, "for-each-ref", "refs/replace/"); refs == "" {
		t.Fatal("the mirror has no replacement ref: the test proves nothing")
	}
	if got := gitRun(t, mirror, "show", commit+":a.txt"); got != "replacement payload" {
		t.Fatalf("with replacements on, git reads %q", got)
	}
}
