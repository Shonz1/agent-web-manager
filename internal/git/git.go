// Package git reads what has changed in a working tree, using the git CLI.
//
// A sandbox's workspace is bind-mounted from the host, so the files an agent
// is editing inside it are these very files. Reading the diff here rather than
// through "sbx exec" keeps it working while the sandbox is stopped, costs
// nothing when it is not being looked at, and does not depend on git being
// installed in the agent's image.
//
// Reading is all that showing a diff does: every command behind one is a read,
// and none of them takes the index lock, so looking at a diff can never disturb
// an agent that is halfway through a commit in the same repository. Adding a
// worktree is the one thing here that writes, and it is confined to
// worktree.go — asked for outright, never on the way to a view.
package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrNotRepo is returned for a directory that is not inside a git checkout.
var ErrNotRepo = errors.New("not a git repository")

// ErrNoSuchFile is returned when a path is not one of the changed files.
var ErrNoSuchFile = errors.New("no such changed file")

const (
	// emptyTree is git's fixed hash for a tree with nothing in it. A repository
	// with no commits has no HEAD to compare against, and this is what stands
	// in for one: every file in it then reads as added, which is what it is.
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	// maxOutput caps what is read back from one git command. A generated file
	// nobody meant to commit can be enormous, and the browser has to be handed
	// something it can render either way.
	maxOutput = 4 << 20

	// maxFiles caps a change list, and maxLines a single file's diff. Both are
	// far past what anyone reads and well short of what stalls a page.
	maxFiles = 2000
	maxLines = 4000

	// binarySniff is how far into a file to look for a NUL before calling it
	// binary — the same heuristic git itself uses.
	binarySniff = 8000
)

// sandboxMarker is printed inside the sandbox, ahead of the command being run
// there, so that what sbx writes to that same stream — "Sandbox … started
// successfully", when the exec had to start the container first — can be told
// from the output being read, and dropped. A diff says nothing about itself
// that could be looked for instead, and glued to that line it parses as
// nonsense.
const sandboxMarker = "--- sandbox output follows ---"

// Base is what the working tree is compared against.
type Base string

const (
	// BaseHead is everything not committed yet: the agent's work in progress.
	BaseHead Base = "head"
	// BaseBranch is everything this branch has changed — commits included —
	// measured from where it left the default branch.
	BaseBranch Base = "branch"
)

// ParseBase maps a query value to a base, falling back to uncommitted changes
// for anything unrecognised.
func ParseBase(s string) Base {
	if Base(s) == BaseBranch {
		return BaseBranch
	}
	return BaseHead
}

// Change is one file that differs from the base.
type Change struct {
	Path string `json:"path"`
	// OldPath is where a renamed file came from, and is what makes the diff of
	// one findable again: git only shows a rename when told both names.
	OldPath string `json:"oldPath,omitempty"`
	// Status is one of added, modified, deleted, renamed, untracked.
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary,omitempty"`
}

// Changes is the whole picture: what the working tree is being compared
// against, and every file that differs from it.
type Changes struct {
	Root   string `json:"root"`
	Branch string `json:"branch,omitempty"`
	Base   Base   `json:"base"`
	// BaseRef names what the comparison is against, for the UI to show: "HEAD"
	// for uncommitted work, or the branch this one grew out of.
	BaseRef   string   `json:"baseRef"`
	Files     []Change `json:"files"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Line is one line of a diff, carrying the numbers it has on each side.
type Line struct {
	// Kind is add, del, ctx, or note — the last being git's own remarks, such
	// as a missing newline at the end of a file.
	Kind string `json:"kind"`
	Old  int    `json:"old,omitempty"`
	New  int    `json:"new,omitempty"`
	Text string `json:"text"`
}

// Hunk is one stretch of a file that changed.
type Hunk struct {
	// Header is what git writes after the line numbers: the enclosing function
	// or section, when it can work one out.
	Header string `json:"header"`
	// Range is the "@@ -1,15 +1,25 @@" git wrote, kept whole so the UI can show
	// the header everyone already reads diffs by.
	Range   string `json:"range"`
	OldLine int    `json:"oldLine"`
	NewLine int    `json:"newLine"`
	Lines   []Line `json:"lines"`
}

// FileDiff is one file's changes, parsed out of git's unified diff.
type FileDiff struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Hunks     []Hunk `json:"hunks"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Client runs git commands. The zero value uses "git" from PATH.
type Client struct {
	Bin string

	// sbxBin and sandbox, when set, run every command inside a sandbox
	// instead of on this machine; see InSandbox.
	sbxBin  string
	sandbox string

	// probes remembers what each repository is, for the few seconds a view
	// spends asking about the same one; see probeCache.
	probes *probeCache
}

func New(bin string) *Client {
	if bin == "" {
		bin = "git"
	}
	return &Client{Bin: bin, probes: newProbeCache()}
}

// InSandbox returns a copy of c that runs its git inside the named sandbox,
// through "sbx exec", rather than on the machine this manager runs on.
//
// It is how a clone-mode sandbox is read. That sandbox's workspace is a git
// clone made inside the container, at the same path the host folder would
// have been mounted at — the host has no copy of it, and reading the host
// folder instead would show the user their own uncommitted work in place of
// what the agent has done.
func (c *Client) InSandbox(sbxBin, sandbox string) *Client {
	in := *c
	if sbxBin == "" {
		sbxBin = "sbx"
	}
	in.sbxBin, in.sandbox = sbxBin, sandbox
	return &in
}

// remote reports whether this client's git runs somewhere other than here.
func (c *Client) remote() bool { return c.sandbox != "" }

// Changes lists every file in dir's repository that differs from base.
func (c *Client) Changes(ctx context.Context, dir string, base Base) (Changes, error) {
	info, err := c.probe(ctx, dir)
	if err != nil {
		return Changes{}, err
	}
	rev, ref := baseFor(info, base)

	out := Changes{Root: info.Root, Branch: info.Branch, Base: base, BaseRef: ref, Files: []Change{}}

	files, err := c.changedFiles(ctx, info.Root, rev)
	if err != nil {
		return Changes{}, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) > maxFiles {
		files = files[:maxFiles]
		out.Truncated = true
	}
	// Counting an untracked file is a read of its own, so it is left until the
	// list has been cut down to the files that will be shown: a sandbox full
	// of build output has tens of thousands of them, and nobody is going to
	// see past the first two thousand.
	c.countUntracked(ctx, info.Root, files)
	out.Files = files
	return out, nil
}

// changedFiles gathers the three lists a change list is built from: what
// differs from rev, how much of each differs, and what is sitting there that
// git has never been told about.
//
// Inside a sandbox all three are read by one script, because the cost of
// reading them is the container round trip and not the git: three commands
// that each take milliseconds were paying for three of those.
func (c *Client) changedFiles(ctx context.Context, root, rev string) ([]Change, error) {
	var status, numstat, others []byte
	if c.remote() {
		// The label is the first of the commands the script runs, so that a
		// failure still names something the reader can go and run.
		out, cut, err := c.capture(ctx, "diff --name-status", false, func(ctx context.Context) *exec.Cmd {
			return c.sandboxCommand(ctx, listScript, root, rev)
		})
		if err != nil {
			return nil, err
		}
		secs, err := sections(out, cut)
		if err != nil {
			return nil, err
		}
		status, numstat, others = secs["status"], secs["numstat"], secs["others"]
	} else {
		var err error
		if status, _, err = c.output(ctx, root, "diff", "--name-status", "-z", "--no-color", rev, "--"); err != nil {
			return nil, err
		}
		if numstat, _, err = c.output(ctx, root, "diff", "--numstat", "-z", "--no-color", rev, "--"); err != nil {
			return nil, err
		}
		if others, _, err = c.output(ctx, root, "ls-files", "--others", "--exclude-standard", "-z"); err != nil {
			return nil, err
		}
	}

	files, err := parseNameStatus(status)
	if err != nil {
		return nil, err
	}
	if err := applyNumstat(numstat, files); err != nil {
		return nil, err
	}
	// A file the agent has just created is untracked, and git's diff machinery
	// says nothing about one — but a new file is the most interesting thing on
	// the list, so it is gathered separately and counted by hand.
	for _, name := range splitZ(others) {
		files = append(files, Change{Path: name, Status: "untracked"})
	}
	return files, nil
}

// FileDiff returns one file's diff against base. oldPath is where the file was
// renamed from, and may be empty; without it git shows a rename as a file
// added out of nowhere.
func (c *Client) FileDiff(ctx context.Context, dir string, base Base, file, oldPath string) (FileDiff, error) {
	if err := validPath(file); err != nil {
		return FileDiff{}, err
	}
	if oldPath != "" {
		if err := validPath(oldPath); err != nil {
			return FileDiff{}, err
		}
	}

	info, err := c.probe(ctx, dir)
	if err != nil {
		return FileDiff{}, err
	}
	rev, _ := baseFor(info, base)

	out := FileDiff{Path: file, OldPath: oldPath, Hunks: []Hunk{}}

	raw, truncated, err := c.fileDiff(ctx, info.Root, rev, file, oldPath)
	if err != nil {
		return FileDiff{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return FileDiff{}, fmt.Errorf("%w: %s", ErrNoSuchFile, file)
	}

	hunks, binary, cut := parseUnified(raw)
	if hunks != nil {
		out.Hunks = hunks
	}
	out.Binary = binary
	out.Truncated = truncated || cut
	return out, nil
}

// fileDiff reads one file's diff against rev, whichever kind of file it is.
//
// Which kind it is has to be established first, and inside a sandbox that
// question and the diff that follows from it go in one script: asking it
// separately doubled the round trips for every file opened.
func (c *Client) fileDiff(ctx context.Context, root, rev, file, oldPath string) ([]byte, bool, error) {
	if c.remote() {
		return c.capture(ctx, "diff "+file, false, func(ctx context.Context) *exec.Cmd {
			return c.sandboxCommand(ctx, fileScript, root, rev, file, oldPath)
		})
	}
	if c.untracked(ctx, root, file) {
		// An untracked file has nothing in the index to compare against, so it
		// is diffed against nothing at all. --no-index is the one form of git
		// diff that will do that, and it needs no index to do it.
		return c.diffOutput(ctx, root, "diff", "--no-index", "--no-color", "--", os.DevNull, file)
	}
	args := []string{"diff", "--no-color", rev, "--", file}
	if oldPath != "" {
		args = []string{"diff", "--no-color", "--find-renames", rev, "--", oldPath, file}
	}
	return c.output(ctx, root, args...)
}

// --- what a repository is ---

// repoInfo is what a repository is, apart from any particular comparison:
// where its top is, what is on HEAD, what is checked out, and what this branch
// appears to have grown out of.
//
// These are gathered together because they are asked for together, and none of
// them depends on which base is being looked at — so one reading of them
// serves the change list and every file opened from it.
type repoInfo struct {
	Root string
	// Head is the commit HEAD names, or "" in a repository with no commits.
	Head   string
	Branch string
	// Default is the branch this one appears to have grown out of, and
	// MergeBase the commit where it left it. Both are "" when there is nothing
	// to have branched from.
	Default   string
	MergeBase string
}

// probe reads what dir's repository is, or returns what it was a moment ago.
func (c *Client) probe(ctx context.Context, dir string) (repoInfo, error) {
	if dir == "" {
		return repoInfo{}, ErrNotRepo
	}
	// The sandbox belongs in the key: a clone sandbox's workspace has the same
	// path as the host folder it was cloned from, and is a different checkout.
	key := c.sandbox + "\x00" + dir
	if info, ok := c.probes.get(key); ok {
		return info, nil
	}
	info, err := c.readRepo(ctx, dir)
	if err != nil {
		return repoInfo{}, err
	}
	c.probes.put(key, info)
	return info, nil
}

func (c *Client) readRepo(ctx context.Context, dir string) (repoInfo, error) {
	if c.remote() {
		return c.probeInSandbox(ctx, dir)
	}
	return c.probeHere(ctx, dir)
}

// probeInSandbox reads a repository through one "sbx exec" rather than the six
// or so commands it takes, which on this side of a container is the whole cost.
func (c *Client) probeInSandbox(ctx context.Context, dir string) (repoInfo, error) {
	out, _, err := c.capture(ctx, "rev-parse --show-toplevel", false, func(ctx context.Context) *exec.Cmd {
		return c.sandboxCommand(ctx, probeScript, dir)
	})
	if err != nil {
		if notRepo(err) {
			return repoInfo{}, ErrNotRepo
		}
		// A sandbox that would not start, or a git that is not installed:
		// answering ErrNotRepo would have the UI explain that the workspace is
		// not a checkout, which is a confident answer to a question nothing
		// managed to ask.
		return repoInfo{}, err
	}
	secs, err := sections(out, false)
	if err != nil {
		return repoInfo{}, err
	}
	info := repoInfo{
		Root:      trimmed(secs["root"]),
		Head:      trimmed(secs["head"]),
		Branch:    trimmed(secs["branch"]),
		Default:   trimmed(secs["default"]),
		MergeBase: trimmed(secs["mergebase"]),
	}
	if info.Root == "" {
		return repoInfo{}, ErrNotRepo
	}
	return info, nil
}

// probeHere reads a repository on this machine, where a git command costs
// milliseconds and there is nothing to be gained by bundling them.
func (c *Client) probeHere(ctx context.Context, dir string) (repoInfo, error) {
	// Only when the checkout is on this machine. A sandbox's own clone sits
	// at this path inside the container, where this stat says nothing about
	// whether it is there.
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return repoInfo{}, fmt.Errorf("workspace %q is not readable from here", dir)
	}
	out, _, err := c.output(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if notRepo(err) {
			return repoInfo{}, ErrNotRepo
		}
		return repoInfo{}, err
	}
	info := repoInfo{Root: strings.TrimSpace(string(out))}
	if info.Root == "" {
		return repoInfo{}, ErrNotRepo
	}

	// Past the root, everything is allowed to come back empty: a repository
	// with no commits has no HEAD and no branch, and one that was never cloned
	// has nothing it grew out of. None of those is a failure to read it.
	if out, _, err := c.output(ctx, info.Root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		info.Head = strings.TrimSpace(string(out))
	}
	if out, _, err := c.output(ctx, info.Root, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		info.Branch = strings.TrimSpace(string(out))
	}
	info.Default = c.defaultBranch(ctx, info.Root)
	if info.Default != "" && info.Head != "" {
		if out, _, err := c.output(ctx, info.Root, "merge-base", info.Default, "HEAD"); err == nil {
			info.MergeBase = strings.TrimSpace(string(out))
		}
	}
	return info, nil
}

func trimmed(b []byte) string { return strings.TrimSpace(string(b)) }

// notRepo reports whether a git command failed because there is no repository
// where it was pointed, rather than failing to run at all. git says which in
// its own words, and LC_ALL=C is set so the words are these.
func notRepo(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "not a working tree")
}

// Branch returns the branch currently checked out in dir, or "" if dir is
// not a git checkout, has no commits yet, or is in a detached HEAD state —
// callers that only want something to show beside a name treat all of those
// alike rather than as errors.
func (c *Client) Branch(ctx context.Context, dir string) string {
	info, err := c.probe(ctx, dir)
	if err != nil {
		return ""
	}
	return info.Branch
}

// IsRepo reports whether dir is inside a git checkout. It is what decides
// whether a sandbox can be given a clone of it: there is nothing to clone
// from a plain directory, and sbx would refuse.
func (c *Client) IsRepo(ctx context.Context, dir string) bool {
	_, err := c.probe(ctx, dir)
	return err == nil
}

// baseFor turns a base into the revision to diff against and the name to show
// for it. Everything it needs was read once, by probe.
func baseFor(info repoInfo, base Base) (rev, ref string) {
	// A repository with no commits has no HEAD to compare against, and the
	// empty tree stands in for one: every file in it then reads as added.
	head, headRef := emptyTree, "the empty tree"
	if info.Head != "" {
		head, headRef = "HEAD", "HEAD"
	}
	if base != BaseBranch || info.Head == "" {
		return head, headRef
	}
	if info.Default == "" || info.MergeBase == "" {
		// Nothing to have branched from — a repository with one branch, or one
		// whose default is named something this cannot guess. Uncommitted work
		// is still worth showing, and the name says which it is.
		return head, headRef
	}
	// This branch is the default one, or has not left it yet: there is no
	// stretch of commits to show, and saying "main since main" would only be a
	// confusing way of saying that. What is uncommitted is still worth seeing.
	if info.MergeBase == info.Head {
		return head, headRef
	}
	return info.MergeBase, info.Default
}

// defaultBranch works out which branch this one grew out of, on this machine.
// What the remote says is authoritative; the usual names are only guessed at
// when it says nothing. Inside a sandbox the same walk happens in probeScript.
func (c *Client) defaultBranch(ctx context.Context, root string) string {
	if out, _, err := c.output(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}
	for _, name := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, _, err := c.output(ctx, root, "rev-parse", "--verify", "--quiet", name+"^{commit}"); err == nil {
			return name
		}
	}
	return ""
}

// untracked reports whether git is ignoring this file because it has never
// been added.
func (c *Client) untracked(ctx context.Context, root, file string) bool {
	out, _, err := c.output(ctx, root, "ls-files", "--others", "--exclude-standard", "-z", "--", file)
	return err == nil && len(bytes.TrimRight(out, "\x00")) > 0
}

// output runs a git command in dir and returns its stdout, saying whether it
// had to be cut short at maxOutput.
func (c *Client) output(ctx context.Context, dir string, args ...string) ([]byte, bool, error) {
	return c.run(ctx, dir, false, args...)
}

// diffOutput is output for the one command that reports its findings through
// its exit status: "git diff --no-index" exits 1 to say the two files differ,
// which is precisely what it is being asked. A real failure still names itself
// on stderr, and that is what tells the two apart.
func (c *Client) diffOutput(ctx context.Context, dir string, args ...string) ([]byte, bool, error) {
	return c.run(ctx, dir, true, args...)
}

func (c *Client) run(ctx context.Context, dir string, differencesOK bool, args ...string) ([]byte, bool, error) {
	return c.capture(ctx, strings.Join(args, " "), differencesOK, func(ctx context.Context) *exec.Cmd {
		return c.command(ctx, dir, args)
	})
}

// capture runs one command and returns its stdout, saying whether it had to be
// cut short at maxOutput. The command is built here rather than handed in
// ready-made because cutting it short means killing it, and only a command
// built on this context can be killed.
func (c *Client) capture(ctx context.Context, label string, differencesOK bool, build func(context.Context) *exec.Cmd) ([]byte, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := build(ctx)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("git %s: %w", label, err)
	}

	// Read one byte past the cap, so a diff that is exactly at it is not
	// reported as cut short.
	data, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	truncated := len(data) > maxOutput
	if truncated {
		data = data[:maxOutput]
		// git is still writing into a pipe nobody is draining; killing it is
		// the only way to get the Wait below to return.
		cancel()
	}
	waitErr := cmd.Wait()

	if truncated {
		out, err := c.fromSandbox(data)
		return out, true, err
	}
	if readErr != nil {
		return nil, false, fmt.Errorf("git %s: %w", label, readErr)
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if differencesOK && errors.As(waitErr, &ee) && ee.ExitCode() == 1 && stderr.Len() == 0 {
			out, err := c.fromSandbox(data)
			return out, false, err
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, false, fmt.Errorf("git %s: %s", label, msg)
		}
		return nil, false, fmt.Errorf("git %s: %w", label, waitErr)
	}
	out, err := c.fromSandbox(data)
	return out, false, err
}

// fromSandbox drops what sbx wrote to stdout ahead of the command it was asked
// to run. Output from this machine is its own, and comes back untouched.
func (c *Client) fromSandbox(data []byte) ([]byte, error) {
	if !c.remote() {
		return data, nil
	}
	marker := []byte(sandboxMarker + "\n")
	i := bytes.Index(data, marker)
	if i < 0 {
		// The command never got as far as its first line, so all that came
		// back is sbx's, and it is the only account of what went wrong.
		if said := strings.TrimSpace(string(data)); said != "" {
			return nil, fmt.Errorf("sandbox %s did not run it: %s", c.sandbox, said)
		}
		return nil, fmt.Errorf("sandbox %s did not run it", c.sandbox)
	}
	return data[i+len(marker):], nil
}

// command builds one git invocation, here or inside a sandbox.
func (c *Client) command(ctx context.Context, dir string, args []string) *exec.Cmd {
	if !c.remote() {
		cmd := exec.CommandContext(ctx, c.bin(), args...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		return cmd
	}
	// "git", not c.bin(): the -git flag names a binary on this machine, and
	// the sandbox has its own. The directory is named with "-C" rather than by
	// running from it, since the working directory of the process spawned here
	// does not reach the other side of "sbx exec".
	return c.sandboxCommand(ctx, `exec "$@"`, append([]string{"git", "-C", dir}, args...)...)
}

// sandboxCommand runs one shell script inside the sandbox, with the arguments
// as its positional parameters and the marker printed ahead of anything it
// writes. The environment is carried as an "env" prefix, since that does not
// cross "sbx exec" either.
func (c *Client) sandboxCommand(ctx context.Context, script string, args ...string) *exec.Cmd {
	argv := append([]string{"exec", c.sandbox, "env"}, gitVars()...)
	// The "sh" after the script is $0: what follows it is $1 onwards, which is
	// where a script expects to find its arguments.
	argv = append(argv, "sh", "-c", fmt.Sprintf("printf '%%s\\n' %q\n%s", sandboxMarker, script), "sh")
	return exec.CommandContext(ctx, c.sbxBin, append(argv, args...)...)
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "git"
	}
	return c.Bin
}

// gitEnv keeps a read out of the way of whatever else is using the repository.
func gitEnv() []string {
	return append(os.Environ(), gitVars()...)
}

// gitVars is the part of that environment this program sets itself, which is
// the only part worth carrying into a sandbox.
func gitVars() []string {
	return []string{
		// Without this, a plain "git diff" refreshes and rewrites the index,
		// which means taking its lock — behind an agent's back, in a repository
		// it may be committing to at that moment.
		"GIT_OPTIONAL_LOCKS=0",
		// Nothing here can answer a prompt, and a git that waits for one would
		// hang until the request's own deadline.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	}
}

// --- parsing ---

// splitZ splits NUL-terminated output into its fields.
func splitZ(data []byte) []string {
	var out []string
	for _, f := range bytes.Split(data, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}

// statusWord turns git's letter into something the UI can show. Renames and
// copies carry a similarity score after the letter.
func statusWord(code string) string {
	if code == "" {
		return "modified"
	}
	switch code[0] {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'T':
		return "typechange"
	default:
		return "modified"
	}
}

// parseNameStatus reads "diff --name-status -z": a status field, then the one
// path it applies to — or, for a rename or a copy, the path it came from and
// the path it went to.
func parseNameStatus(data []byte) ([]Change, error) {
	fields := splitZ(data)
	out := make([]Change, 0, len(fields))
	for i := 0; i < len(fields); {
		code := fields[i]
		i++
		twoPaths := code != "" && (code[0] == 'R' || code[0] == 'C')
		need := 1
		if twoPaths {
			need = 2
		}
		if i+need > len(fields) {
			return nil, fmt.Errorf("truncated name-status record %q", code)
		}
		ch := Change{Status: statusWord(code)}
		if twoPaths {
			ch.OldPath, ch.Path = fields[i], fields[i+1]
		} else {
			ch.Path = fields[i]
		}
		i += need
		out = append(out, ch)
	}
	return out, nil
}

// applyNumstat fills in the line counts from "diff --numstat -z", which writes
// "<added>\t<removed>\t<path>" per file — except for a rename, where the path
// is left empty and the two names follow as fields of their own. A binary file
// has "-" for both counts.
func applyNumstat(data []byte, changes []Change) error {
	byPath := make(map[string]*Change, len(changes))
	for i := range changes {
		byPath[changes[i].Path] = &changes[i]
	}

	fields := splitZ(data)
	for i := 0; i < len(fields); {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("malformed numstat record %q", fields[i])
		}
		i++

		file := parts[2]
		if file == "" {
			// A rename: the old and new names are the next two fields.
			if i+2 > len(fields) {
				return fmt.Errorf("truncated numstat rename record")
			}
			file = fields[i+1]
			i += 2
		}

		ch, ok := byPath[file]
		if !ok {
			continue
		}
		if parts[0] == "-" || parts[1] == "-" {
			ch.Binary = true
			continue
		}
		ch.Added, _ = strconv.Atoi(parts[0])
		ch.Removed, _ = strconv.Atoi(parts[1])
	}
	return nil
}

// parseUnified turns git's diff into hunks of numbered lines. Everything
// before the first hunk header is git's preamble — which names the file, and
// which the caller already knows — apart from the one line that says the file
// is binary and has no hunks at all.
func parseUnified(data []byte) (hunks []Hunk, binary, truncated bool) {
	var cur *Hunk
	var oldNo, newNo int
	lines := 0

	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := string(raw)
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// The preamble of another file. Asking for a rename hands git two
			// paths, and it answers with two sections when it decides they are
			// two files after all.
			cur = nil
			continue
		case strings.HasPrefix(line, "@@"):
			h, o, n, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			hunks = append(hunks, h)
			cur = &hunks[len(hunks)-1]
			oldNo, newNo = o, n
			continue
		case cur == nil:
			if strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch") {
				binary = true
			}
			continue
		}

		if lines >= maxLines {
			truncated = true
			break
		}

		switch {
		case strings.HasPrefix(line, "+"):
			cur.Lines = append(cur.Lines, Line{Kind: "add", New: newNo, Text: line[1:]})
			newNo++
		case strings.HasPrefix(line, "-"):
			cur.Lines = append(cur.Lines, Line{Kind: "del", Old: oldNo, Text: line[1:]})
			oldNo++
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file" — about the line before it, and
			// numbered as part of neither side.
			cur.Lines = append(cur.Lines, Line{Kind: "note", Text: strings.TrimSpace(line[1:])})
			continue
		case line == "":
			// The trailing newline of the output, not a line of the file.
			continue
		default: // a context line, which git prefixes with a space
			cur.Lines = append(cur.Lines, Line{Kind: "ctx", Old: oldNo, New: newNo, Text: line[1:]})
			oldNo++
			newNo++
		}
		lines++
	}
	return hunks, binary, truncated
}

// parseHunkHeader reads "@@ -old,count +new,count @@ section".
func parseHunkHeader(line string) (h Hunk, oldNo, newNo int, ok bool) {
	end := strings.Index(line[2:], "@@")
	if end < 0 {
		return Hunk{}, 0, 0, false
	}
	ranges := strings.Fields(line[2 : 2+end])
	if len(ranges) < 2 {
		return Hunk{}, 0, 0, false
	}
	oldNo = rangeStart(ranges[0], '-')
	newNo = rangeStart(ranges[1], '+')
	if oldNo < 0 || newNo < 0 {
		return Hunk{}, 0, 0, false
	}
	return Hunk{
		Header:  strings.TrimSpace(line[2+end+2:]),
		Range:   strings.TrimSpace(line[:2+end+2]),
		OldLine: oldNo,
		NewLine: newNo,
		Lines:   []Line{},
	}, oldNo, newNo, true
}

// rangeStart reads the first number of a "-12,7" or "+12" range.
func rangeStart(field string, sign byte) int {
	if field == "" || field[0] != sign {
		return -1
	}
	num, _, _ := strings.Cut(field[1:], ",")
	n, err := strconv.Atoi(num)
	if err != nil {
		return -1
	}
	return n
}

// countUntracked fills in what each untracked file on the list adds, and marks
// the ones nobody wants rendered as text.
//
// A file on this machine is read directly. The ones inside a sandbox are
// counted by the git that can see them, in a single pass: an "sbx exec" costs
// more than every count it carries put together, and one per file would spend
// the whole request's budget before the list was drawn. A count that cannot be
// had leaves the file at no lines rather than failing the list — it is a
// number beside a filename, and the filename is the part that matters.
func (c *Client) countUntracked(ctx context.Context, root string, files []Change) {
	var names []string
	for i := range files {
		if files[i].Status != "untracked" {
			continue
		}
		if !c.remote() {
			files[i].Added, files[i].Binary = countLines(filepath.Join(root, files[i].Path))
			continue
		}
		names = append(names, files[i].Path)
	}
	if len(names) == 0 {
		return
	}

	out, _, err := c.capture(ctx, "diff --no-index --numstat", false, func(ctx context.Context) *exec.Cmd {
		return c.sandboxCommand(ctx, countScript, append([]string{root}, names...)...)
	})
	if err != nil {
		return
	}
	_ = applyNumstat(out, files)
}

// countScript counts every untracked file it is handed, in one shell inside
// the sandbox. Each is diffed against nothing, exactly as FileDiff diffs an
// untracked file, so what comes back is git's own numstat — with the two names
// of a rename, /dev/null and the file, since that is what a diff between
// differently named paths is. A file git will not read contributes nothing and
// does not stop the ones after it.
const countScript = `root=$1
shift
for f in "$@"; do
	git -C "$root" diff --no-index --numstat -z -- /dev/null "$f" || true
done`

// countLines counts what an untracked file adds, and says whether it is one
// nobody wants rendered as text. Everything in such a file is an addition, so
// there is nothing else to work out.
func countLines(name string) (int, bool) {
	f, err := os.Open(name)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	r := bufio.NewReader(f)
	count, read := 0, 0
	binary := false
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if !binary && read < binarySniff {
				sniff := chunk
				if over := read + n - binarySniff; over > 0 {
					sniff = chunk[:n-over]
				}
				binary = bytes.IndexByte(sniff, 0) >= 0
			}
			if binary {
				return 0, true
			}
			count += bytes.Count(chunk, []byte("\n"))
			read += n
		}
		if err != nil {
			break
		}
	}
	return count, false
}

// validPath rejects anything that is not a plain path inside the repository.
// The paths handed here come from a URL, and one of the commands they reach is
// "git diff --no-index", which will happily read a file anywhere on the disk.
func validPath(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if len(p) > 4096 {
		return errors.New("path is too long")
	}
	if strings.ContainsRune(p, 0) {
		return errors.New("path contains a NUL")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return fmt.Errorf("path %q is not relative to the repository", p)
	}
	// Cleaned with the slash-separated form: these come from git, which uses
	// forward slashes on every platform.
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q leaves the repository", p)
	}
	return nil
}
