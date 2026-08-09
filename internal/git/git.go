// Package git reads what has changed in a working tree, using the git CLI.
//
// A sandbox's workspace is bind-mounted from the host, so the files an agent
// is editing inside it are these very files. Reading the diff here rather than
// through "sbx exec" keeps it working while the sandbox is stopped, costs
// nothing when it is not being looked at, and does not depend on git being
// installed in the agent's image.
//
// Nothing in here writes: every command is a read, and none of them takes the
// index lock, so looking at a diff can never disturb an agent that is halfway
// through a commit in the same repository.
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
}

func New(bin string) *Client {
	if bin == "" {
		bin = "git"
	}
	return &Client{Bin: bin}
}

// Changes lists every file in dir's repository that differs from base.
func (c *Client) Changes(ctx context.Context, dir string, base Base) (Changes, error) {
	root, err := c.root(ctx, dir)
	if err != nil {
		return Changes{}, err
	}

	rev, ref, err := c.resolveBase(ctx, root, base)
	if err != nil {
		return Changes{}, err
	}

	out := Changes{Root: root, Base: base, BaseRef: ref, Files: []Change{}}
	out.Branch, _ = c.branch(ctx, root)

	status, _, err := c.output(ctx, root, "diff", "--name-status", "-z", "--no-color", rev, "--")
	if err != nil {
		return Changes{}, err
	}
	files, err := parseNameStatus(status)
	if err != nil {
		return Changes{}, err
	}

	numstat, _, err := c.output(ctx, root, "diff", "--numstat", "-z", "--no-color", rev, "--")
	if err != nil {
		return Changes{}, err
	}
	if err := applyNumstat(numstat, files); err != nil {
		return Changes{}, err
	}

	// A file the agent has just created is untracked, and git's diff machinery
	// says nothing about one — but a new file is the most interesting thing on
	// the list, so it is gathered separately and counted by hand.
	others, _, err := c.output(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Changes{}, err
	}
	for _, name := range splitZ(others) {
		added, binary := countLines(filepath.Join(root, name))
		files = append(files, Change{Path: name, Status: "untracked", Added: added, Binary: binary})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) > maxFiles {
		files = files[:maxFiles]
		out.Truncated = true
	}
	out.Files = files
	return out, nil
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

	root, err := c.root(ctx, dir)
	if err != nil {
		return FileDiff{}, err
	}

	out := FileDiff{Path: file, OldPath: oldPath, Hunks: []Hunk{}}

	var raw []byte
	var truncated bool
	if c.untracked(ctx, root, file) {
		// An untracked file has nothing in the index to compare against, so it
		// is diffed against nothing at all. --no-index is the one form of git
		// diff that will do that, and it needs no index to do it.
		raw, truncated, err = c.diffOutput(ctx, root, "diff", "--no-index", "--no-color", "--", os.DevNull, file)
	} else {
		rev, _, rerr := c.resolveBase(ctx, root, base)
		if rerr != nil {
			return FileDiff{}, rerr
		}
		args := []string{"diff", "--no-color", rev, "--", file}
		if oldPath != "" {
			args = []string{"diff", "--no-color", "--find-renames", rev, "--", oldPath, file}
		}
		raw, truncated, err = c.output(ctx, root, args...)
	}
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

// --- git invocations ---

// root returns the top of the checkout dir is in.
func (c *Client) root(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		return "", ErrNotRepo
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not readable from here", dir)
	}
	out, _, err := c.output(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ErrNotRepo
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", ErrNotRepo
	}
	return root, nil
}

func (c *Client) branch(ctx context.Context, root string) (string, error) {
	out, _, err := c.output(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveBase turns a base into the revision to diff against and the name to
// show for it.
func (c *Client) resolveBase(ctx context.Context, root string, base Base) (rev, ref string, err error) {
	head := emptyTree
	headRef := "the empty tree"
	if _, _, err := c.output(ctx, root, "rev-parse", "--verify", "HEAD^{commit}"); err == nil {
		head, headRef = "HEAD", "HEAD"
	}
	if base != BaseBranch || head == emptyTree {
		return head, headRef, nil
	}

	def, ok := c.defaultBranch(ctx, root)
	if !ok {
		// Nothing to have branched from — a repository with one branch, or one
		// whose default is named something this cannot guess. Uncommitted work
		// is still worth showing, and the name says which it is.
		return head, headRef, nil
	}
	out, _, err := c.output(ctx, root, "merge-base", def, "HEAD")
	if err != nil {
		return head, headRef, nil
	}
	mergeBase := strings.TrimSpace(string(out))
	if mergeBase == "" {
		return head, headRef, nil
	}
	// This branch is the default one, or has not left it yet: there is no
	// stretch of commits to show, and saying "main since main" would only be a
	// confusing way of saying that. What is uncommitted is still worth seeing.
	if headOID, _, err := c.output(ctx, root, "rev-parse", "HEAD"); err == nil {
		if strings.TrimSpace(string(headOID)) == mergeBase {
			return head, headRef, nil
		}
	}
	return mergeBase, def, nil
}

// defaultBranch works out which branch this one grew out of. What the remote
// says is authoritative; the usual names are only guessed at when it says
// nothing.
func (c *Client) defaultBranch(ctx context.Context, root string) (string, bool) {
	if out, _, err := c.output(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name, true
		}
	}
	for _, name := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, _, err := c.output(ctx, root, "rev-parse", "--verify", name+"^{commit}"); err == nil {
			return name, true
		}
	}
	return "", false
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("git %s: %w", args[0], err)
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
		return data, true, nil
	}
	if readErr != nil {
		return nil, false, fmt.Errorf("git %s: %w", args[0], readErr)
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if differencesOK && errors.As(waitErr, &ee) && ee.ExitCode() == 1 && stderr.Len() == 0 {
			return data, false, nil
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, false, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
		}
		return nil, false, fmt.Errorf("git %s: %w", strings.Join(args, " "), waitErr)
	}
	return data, false, nil
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "git"
	}
	return c.Bin
}

// gitEnv keeps a read out of the way of whatever else is using the repository.
func gitEnv() []string {
	return append(os.Environ(),
		// Without this, a plain "git diff" refreshes and rewrites the index,
		// which means taking its lock — behind an agent's back, in a repository
		// it may be committing to at that moment.
		"GIT_OPTIONAL_LOCKS=0",
		// Nothing here can answer a prompt, and a git that waits for one would
		// hang until the request's own deadline.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	)
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
