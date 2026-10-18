package proto

import (
	"os/exec"
	"testing"
)

// ValidBranch agrees with `git check-ref-format --branch` on names git
// accepts and refuses, and also refuses what git would take as an option.
func TestValidBranchMatchesGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	for _, name := range []string{
		"main", "feature/x", "release-1.2", "a.b", "über", "x@y", "-x", "--recurse-submodules", "a/-b",
		"", "HEAD", "@", "a..b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[b", `a\b`, "a@{1}", "/a", "a/", "a//b",
		"a.", ".a", "a/.b", "a.lock", "a/b.lock/c", "a\x01b", "a\x7fb", "refs/heads/main",
	} {
		// `git check-ref-format --branch` also expands @{-N}, and accepts
		// HEAD and @, which git's revision syntax reads as HEAD.
		out := exec.Command(git, "check-ref-format", "--branch", name).Run() == nil
		gitOK := out && name != "HEAD" && name != "@" && name[0] != '-'
		if got := ValidBranch(name); got != gitOK {
			t.Errorf("ValidBranch(%q) = %v; git accepts: %v", name, got, out)
		}
	}
}
