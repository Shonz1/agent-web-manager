package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddWorktreeCreatesBranchBesideTheRepo(t *testing.T) {
	dir := repo(t)
	c := New("")

	tree, err := c.AddWorktree(context.Background(), dir, "", "feature/login")
	if err != nil {
		t.Fatal(err)
	}

	// The branch's slash is one name to git and two to a filesystem, so the
	// directory it goes in is not simply the branch.
	want := dir + "-feature-login"
	if tree.Path != want {
		t.Fatalf("worktree path is %q, want %q", tree.Path, want)
	}
	if tree.Branch != "feature/login" {
		t.Fatalf("branch is %q, want feature/login", tree.Branch)
	}
	if tree.RepoRoot != dir {
		t.Fatalf("repo root is %q, want %q", tree.RepoRoot, dir)
	}

	if info, err := os.Stat(filepath.Join(tree.Path, "keep.txt")); err != nil || info.IsDir() {
		t.Fatalf("the worktree does not hold the repository's files: %v", err)
	}
	if got := strings.TrimSpace(run(t, tree.Path, "rev-parse", "--abbrev-ref", "HEAD")); got != "feature/login" {
		t.Fatalf("worktree is on %q, want feature/login", got)
	}
}

// A worktree's ".git" is a file pointing into the main repository, so the
// repository has to be reported: whoever mounts the worktree has to mount that
// as well or the agent gets no git at all.
func TestAddWorktreeReportsTheMainRepoFromAnotherWorktree(t *testing.T) {
	dir := repo(t)
	c := New("")

	first, err := c.AddWorktree(context.Background(), dir, "", "one")
	if err != nil {
		t.Fatal(err)
	}
	// Branched from the worktree rather than from the repository it came from.
	second, err := c.AddWorktree(context.Background(), first.Path, "", "two")
	if err != nil {
		t.Fatal(err)
	}
	if second.RepoRoot != dir {
		t.Fatalf("repo root is %q, want the main worktree %q", second.RepoRoot, dir)
	}
	if second.Path != dir+"-two" {
		t.Fatalf("worktree path is %q, want %q", second.Path, dir+"-two")
	}
}

// A branch that is already there is checked out rather than refused: the caller
// asked for a worktree on that branch, and it now has one.
func TestAddWorktreeTakesAnExistingBranch(t *testing.T) {
	dir := repo(t)
	run(t, dir, "branch", "already-here")

	tree, err := New("").AddWorktree(context.Background(), dir, "", "already-here")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(run(t, tree.Path, "rev-parse", "--abbrev-ref", "HEAD")); got != "already-here" {
		t.Fatalf("worktree is on %q, want already-here", got)
	}
}

func TestAddWorktreeHonoursAGivenPath(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(t.TempDir(), "somewhere-else")

	tree, err := New("").AddWorktree(context.Background(), dir, path, "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Path != path {
		t.Fatalf("worktree path is %q, want %q", tree.Path, path)
	}
}

// An empty directory is one git would take over, which is not the same as one
// nobody is using.
func TestAddWorktreeRefusesAPathThatExists(t *testing.T) {
	dir := repo(t)
	path := filepath.Join(t.TempDir(), "taken")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := New("").AddWorktree(context.Background(), dir, path, "nope")
	if err == nil {
		t.Fatal("adding a worktree over an existing directory succeeded")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error does not say the directory is there: %v", err)
	}
}

func TestAddWorktreeNeedsARepo(t *testing.T) {
	if _, err := New("").AddWorktree(context.Background(), t.TempDir(), "", "branch"); err == nil {
		t.Fatal("adding a worktree outside a repository succeeded")
	}
}

func TestRemoveWorktreeTakesTheDirectoryWithIt(t *testing.T) {
	dir := repo(t)
	c := New("")

	tree, err := c.AddWorktree(context.Background(), dir, "", "rolled-back")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveWorktree(context.Background(), tree.RepoRoot, tree.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree.Path); !os.IsNotExist(err) {
		t.Fatalf("%s is still there: %v", tree.Path, err)
	}
	// The branch stays behind on purpose: a retry checks it out rather than
	// failing on a name that is already taken.
	if !c.hasBranch(context.Background(), dir, "rolled-back") {
		t.Fatal("removing the worktree took its branch with it")
	}
}

func TestValidBranchRejectsWhatGitRejects(t *testing.T) {
	for _, name := range []string{
		"", "@", "-dashed", "/leading", "trailing/", "double//slash",
		".hidden", "nested/.hidden", "trailing.", "two..dots", "ref.lock",
		"with space", "carets^", "tilde~", "colon:", "question?", "star*",
		"bracket[", "back\\slash", "at@{one}", "new\nline",
	} {
		if err := validBranch(name); err == nil {
			t.Errorf("branch name %q was accepted", name)
		}
	}
	for _, name := range []string{"main", "feature/login", "fix-123", "v1.2.x", "user/JIRA-4_thing"} {
		if err := validBranch(name); err != nil {
			t.Errorf("branch name %q was refused: %v", name, err)
		}
	}
}

// The UI previews the default path with the same rule, so the rule has to be
// worth previewing: no slashes out of the branch, nothing left dangling.
func TestDefaultWorktreePath(t *testing.T) {
	for _, tc := range []struct{ root, branch, want string }{
		{"/src/app", "feature-x", "/src/app-feature-x"},
		{"/src/app/", "feature/x", "/src/app-feature-x"},
		{"/src/app", "...", "/src/app-worktree"},
		{"/src/app", "JIRA-4_fix.thing", "/src/app-JIRA-4_fix.thing"},
	} {
		if got := DefaultWorktreePath(tc.root, tc.branch); got != tc.want {
			t.Errorf("DefaultWorktreePath(%q, %q) = %q, want %q", tc.root, tc.branch, got, tc.want)
		}
	}
}
