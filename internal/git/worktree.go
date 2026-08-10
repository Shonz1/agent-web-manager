package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Worktree is a branch checked out in a directory of its own.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// RepoRoot is the main working tree of the repository this worktree belongs
	// to. It matters to whoever mounts one: a worktree is not self-contained.
	// Its ".git" is a file naming the main repository's administrative
	// directory by absolute path, so anything handed the worktree on its own
	// has no git in it at all.
	RepoRoot string `json:"repoRoot"`
}

// AddWorktree checks branch out into a directory of its own, taken from the
// repository dir is in. An empty path puts it beside that repository; a branch
// that does not exist yet is created from where the repository stands now.
//
// This writes, which nothing else here does: it adds a branch and a directory,
// and records the worktree in the main repository. It takes no index lock and
// touches nothing already checked out, so an agent working in the repository at
// the time is not disturbed by it — but it is asked for outright rather than
// happening behind a view.
func (c *Client) AddWorktree(ctx context.Context, dir, path, branch string) (Worktree, error) {
	branch = strings.TrimSpace(branch)
	if err := validBranch(branch); err != nil {
		return Worktree{}, err
	}

	info, err := c.probe(ctx, dir)
	if err != nil {
		return Worktree{}, err
	}
	root := info.Root
	repoRoot, err := c.mainRoot(ctx, root)
	if err != nil {
		return Worktree{}, err
	}

	if strings.TrimSpace(path) == "" {
		path = DefaultWorktreePath(repoRoot, branch)
	} else {
		abs, err := filepath.Abs(strings.TrimSpace(path))
		if err != nil {
			return Worktree{}, err
		}
		path = abs
	}
	// git refuses a directory that already holds anything, but an empty one it
	// takes over — and a worktree quietly appearing inside a directory someone
	// else is using is not what was asked for.
	if _, err := os.Lstat(path); err == nil {
		return Worktree{}, fmt.Errorf("%s already exists; a worktree needs a directory of its own", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Worktree{}, err
	}

	// A branch that is already there is checked out as it stands; asking for it
	// to be created again would only fail. Which of the two happened is not
	// worth reporting: either way the worktree is on the branch that was asked
	// for.
	args := []string{"worktree", "add", path, branch}
	if !c.hasBranch(ctx, root, branch) {
		args = []string{"worktree", "add", "-b", branch, path, "HEAD"}
	}
	if _, _, err := c.output(ctx, root, args...); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: path, Branch: branch, RepoRoot: repoRoot}, nil
}

// RemoveWorktree undoes an AddWorktree for a caller that could not use what it
// got: a worktree whose sandbox never came up is a checkout nobody asked for,
// and the next attempt at the same branch would fail on the directory this one
// left behind.
//
// It deletes the directory, so it is only ever to be called with a path
// AddWorktree has just returned. The branch stays: it is a name pointing at a
// commit that was already there, and a retry checks it out rather than failing
// on it.
func (c *Client) RemoveWorktree(ctx context.Context, repoRoot, path string) error {
	_, _, err := c.output(ctx, repoRoot, "worktree", "remove", "--force", path)
	return err
}

// mainRoot returns the main working tree of root's repository, which is root
// itself unless root is a worktree of something else.
func (c *Client) mainRoot(ctx context.Context, root string) (string, error) {
	out, _, err := c.output(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return root, nil
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	// The common directory is the main repository's ".git", and the working tree
	// is what holds it. A bare repository has none, but cannot be a workspace
	// either: root came from "--show-toplevel", which fails there.
	main := filepath.Dir(filepath.Clean(common))
	if main == "" || main == "." || main == string(filepath.Separator) {
		return root, nil
	}
	return main, nil
}

// hasBranch reports whether the repository already has this branch.
func (c *Client) hasBranch(ctx context.Context, root, branch string) bool {
	_, _, err := c.output(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// DefaultWorktreePath is where a worktree goes when nobody says: beside the
// repository it came from, named after it and the branch.
//
// Outside the repository rather than in it, because the point of one here is to
// be mounted into a sandbox of its own — and because a checkout nested inside
// another shows up in it as a directory full of files nobody committed.
func DefaultWorktreePath(repoRoot, branch string) string {
	root := strings.TrimSuffix(repoRoot, string(filepath.Separator))
	return filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+slug(branch))
}

var unsafeInDir = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// slug turns a branch name into something that can be a directory: "feat/login"
// is one name to git and two to a filesystem.
func slug(s string) string {
	out := strings.Trim(unsafeInDir.ReplaceAllString(s, "-"), "-.")
	if out == "" {
		return "worktree"
	}
	return out
}

// badInBranch are the characters git's own check-ref-format rejects, beyond the
// control characters and the multi-character sequences checked for separately.
const badInBranch = " ~^:?*[\\"

// validBranch rejects what git would, without having to start git to find out.
// The name reaches a command line and becomes part of a directory name, so this
// is a guard as much as a courtesy.
func validBranch(name string) error {
	switch {
	case name == "":
		return errors.New("a branch name is required")
	case len(name) > 200:
		return errors.New("that branch name is too long")
	case name == "@":
		return errors.New(`"@" is not a branch name git will take`)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("branch name %q cannot start with a dash", name)
	case strings.HasPrefix(name, "/"), strings.HasSuffix(name, "/"), strings.Contains(name, "//"):
		return fmt.Errorf("branch name %q cannot begin, end, or double up on a slash", name)
	case strings.HasPrefix(name, "."), strings.Contains(name, "/."):
		return fmt.Errorf("no part of branch name %q may start with a dot", name)
	case strings.HasSuffix(name, "."), strings.Contains(name, ".."):
		return fmt.Errorf("branch name %q cannot end with a dot or contain two in a row", name)
	case strings.HasSuffix(name, ".lock"):
		return fmt.Errorf("branch name %q cannot end with .lock", name)
	case strings.Contains(name, "@{"):
		return fmt.Errorf("branch name %q cannot contain \"@{\"", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(badInBranch, r) {
			return fmt.Errorf("branch name %q cannot contain %q", name, r)
		}
	}
	return nil
}
