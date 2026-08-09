package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a throwaway checkout with one commit in it and returns its path.
// The tests run the real git, because what is being tested is the shape of
// what git prints — a fake would only assert this file's own assumptions.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	// macOS hands out a symlinked temp directory, which git resolves; the
	// resolved form is what "rev-parse --show-toplevel" comes back with.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	dir = resolved

	run(t, dir, "init", "--initial-branch=main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	write(t, dir, "keep.txt", "one\ntwo\nthree\n")
	write(t, dir, "gone.txt", "delete me\n")
	write(t, dir, "moved.txt", "a\nb\nc\nd\ne\nf\ng\nh\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-m", "initial")
	return dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func changed(t *testing.T, c Changes, path string) Change {
	t.Helper()
	for _, f := range c.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s is not in the change list %+v", path, c.Files)
	return Change{}
}

func TestChangesCoversEveryKindOfChange(t *testing.T) {
	dir := repo(t)
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")
	run(t, dir, "rm", "-q", "gone.txt")
	run(t, dir, "mv", "moved.txt", "renamed.txt")
	write(t, dir, "renamed.txt", "a\nb\nc\nd\ne\nf\ng\nH\n")
	write(t, dir, "fresh.txt", "brand new\nlines\n")
	write(t, dir, "staged.txt", "staged\n")
	run(t, dir, "add", "staged.txt")

	got, err := New("").Changes(context.Background(), dir, BaseHead)
	if err != nil {
		t.Fatal(err)
	}

	if got.Root != dir {
		t.Errorf("root = %s, want %s", got.Root, dir)
	}
	if got.Branch != "main" {
		t.Errorf("branch = %s, want main", got.Branch)
	}
	if got.BaseRef != "HEAD" {
		t.Errorf("baseRef = %s, want HEAD", got.BaseRef)
	}

	// The list is sorted by path, so a file is always in the same place.
	var paths []string
	for _, f := range got.Files {
		paths = append(paths, f.Path)
	}
	want := []string{"fresh.txt", "gone.txt", "keep.txt", "renamed.txt", "staged.txt"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", paths, want)
	}

	if c := changed(t, got, "keep.txt"); c.Status != "modified" || c.Added != 1 || c.Removed != 1 {
		t.Errorf("keep.txt = %+v, want modified +1 -1", c)
	}
	if c := changed(t, got, "gone.txt"); c.Status != "deleted" || c.Removed != 1 {
		t.Errorf("gone.txt = %+v, want deleted -1", c)
	}
	// A rename keeps the name it came from: without it the diff of the file
	// cannot be asked for again.
	if c := changed(t, got, "renamed.txt"); c.Status != "renamed" || c.OldPath != "moved.txt" {
		t.Errorf("renamed.txt = %+v, want renamed from moved.txt", c)
	}
	if c := changed(t, got, "renamed.txt"); c.Added != 1 || c.Removed != 1 {
		t.Errorf("renamed.txt counts = +%d -%d, want +1 -1", c.Added, c.Removed)
	}
	// Untracked and staged files both count as work in progress; the point of
	// the list is that nothing an agent has done is missing from it.
	if c := changed(t, got, "fresh.txt"); c.Status != "untracked" || c.Added != 2 {
		t.Errorf("fresh.txt = %+v, want untracked +2", c)
	}
	if c := changed(t, got, "staged.txt"); c.Status != "added" || c.Added != 1 {
		t.Errorf("staged.txt = %+v, want added +1", c)
	}
}

func TestChangesMarksBinaryFiles(t *testing.T) {
	dir := repo(t)
	write(t, dir, "blob.bin", "text\x00\x01\x02more")
	write(t, dir, "tracked.bin", "\x00\x01")
	run(t, dir, "add", "tracked.bin")
	run(t, dir, "commit", "-m", "binary")
	write(t, dir, "tracked.bin", "\x00\x02")

	got, err := New("").Changes(context.Background(), dir, BaseHead)
	if err != nil {
		t.Fatal(err)
	}
	if c := changed(t, got, "blob.bin"); !c.Binary || c.Added != 0 {
		t.Errorf("blob.bin = %+v, want binary with no counts", c)
	}
	if c := changed(t, got, "tracked.bin"); !c.Binary {
		t.Errorf("tracked.bin = %+v, want binary", c)
	}
}

func TestChangesIgnoresIgnoredFiles(t *testing.T) {
	dir := repo(t)
	write(t, dir, ".gitignore", "build/\n")
	run(t, dir, "add", ".gitignore")
	run(t, dir, "commit", "-m", "ignore build")
	write(t, dir, "build/out.js", "generated\n")

	got, err := New("").Changes(context.Background(), dir, BaseHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 0 {
		t.Fatalf("files = %+v, want none", got.Files)
	}
}

// A repository with nothing committed has no HEAD to compare against. Every
// file in it is new, and that is what should be shown rather than an error.
func TestChangesWithNoCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	dir = resolved
	run(t, dir, "init", "--initial-branch=main")
	write(t, dir, "first.txt", "hello\n")

	got, err := New("").Changes(context.Background(), dir, BaseBranch)
	if err != nil {
		t.Fatal(err)
	}
	if c := changed(t, got, "first.txt"); c.Status != "untracked" {
		t.Errorf("first.txt = %+v, want untracked", c)
	}
}

// Branch mode is the case where the agent has been committing: HEAD sees
// nothing, and the branch still has everything it did.
func TestChangesAgainstBranchIncludesCommits(t *testing.T) {
	dir := repo(t)
	run(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")
	run(t, dir, "commit", "-aqm", "change keep")
	write(t, dir, "later.txt", "not committed\n")

	head, err := New("").Changes(context.Background(), dir, BaseHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(head.Files) != 1 || head.Files[0].Path != "later.txt" {
		t.Fatalf("uncommitted files = %+v, want only later.txt", head.Files)
	}

	branch, err := New("").Changes(context.Background(), dir, BaseBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branch.BaseRef != "main" {
		t.Errorf("baseRef = %s, want main", branch.BaseRef)
	}
	if c := changed(t, branch, "keep.txt"); c.Added != 1 || c.Removed != 1 {
		t.Errorf("keep.txt = %+v, want the committed change counted", c)
	}
	changed(t, branch, "later.txt")
}

// On the default branch there is no stretch of commits to show: "since main"
// while sitting on main says nothing, so it falls back to uncommitted work and
// reports which it settled on.
func TestChangesAgainstBranchOnTheDefaultBranch(t *testing.T) {
	dir := repo(t)
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")
	run(t, dir, "commit", "-aqm", "committed on main")
	write(t, dir, "later.txt", "not committed\n")

	got, err := New("").Changes(context.Background(), dir, BaseBranch)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseRef != "HEAD" {
		t.Errorf("baseRef = %s, want HEAD", got.BaseRef)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "later.txt" {
		t.Errorf("files = %+v, want only the uncommitted one", got.Files)
	}
}

func TestFileDiffHunks(t *testing.T) {
	dir := repo(t)
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")

	got, err := New("").FileDiff(context.Background(), dir, BaseHead, "keep.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(got.Hunks))
	}
	want := []Line{
		{Kind: "ctx", Old: 1, New: 1, Text: "one"},
		{Kind: "del", Old: 2, Text: "two"},
		{Kind: "add", New: 2, Text: "TWO"},
		{Kind: "ctx", Old: 3, New: 3, Text: "three"},
	}
	lines := got.Hunks[0].Lines
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v, want %+v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
		}
	}
}

// An untracked file has nothing in the index to compare against, so it takes
// a different path through git entirely — and it is the most common thing an
// agent leaves behind.
func TestFileDiffOfUntrackedFile(t *testing.T) {
	dir := repo(t)
	write(t, dir, "fresh.txt", "alpha\nbeta\n")

	got, err := New("").FileDiff(context.Background(), dir, BaseHead, "fresh.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hunks) != 1 || len(got.Hunks[0].Lines) != 2 {
		t.Fatalf("hunks = %+v, want one hunk of two added lines", got.Hunks)
	}
	for i, line := range got.Hunks[0].Lines {
		if line.Kind != "add" || line.New != i+1 {
			t.Errorf("line %d = %+v, want an addition numbered %d", i, line, i+1)
		}
	}
}

func TestFileDiffOfRenameNeedsTheOldName(t *testing.T) {
	dir := repo(t)
	run(t, dir, "mv", "moved.txt", "renamed.txt")
	write(t, dir, "renamed.txt", "a\nb\nc\nd\ne\nf\ng\nH\n")

	got, err := New("").FileDiff(context.Background(), dir, BaseHead, "renamed.txt", "moved.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Told both names, git shows the one line that changed rather than a file
	// of eight added lines.
	var adds int
	for _, h := range got.Hunks {
		for _, l := range h.Lines {
			if l.Kind == "add" {
				adds++
			}
		}
	}
	if adds != 1 {
		t.Errorf("added lines = %d, want 1 — the rename was not recognised", adds)
	}
}

func TestFileDiffOfBinaryFile(t *testing.T) {
	dir := repo(t)
	write(t, dir, "blob.bin", "\x00\x01")
	run(t, dir, "add", "blob.bin")
	run(t, dir, "commit", "-m", "binary")
	write(t, dir, "blob.bin", "\x00\x02")

	got, err := New("").FileDiff(context.Background(), dir, BaseHead, "blob.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Binary || len(got.Hunks) != 0 {
		t.Errorf("diff = %+v, want binary with no hunks", got)
	}
}

func TestFileDiffUnknownFile(t *testing.T) {
	dir := repo(t)
	_, err := New("").FileDiff(context.Background(), dir, BaseHead, "keep.txt", "")
	if !errors.Is(err, ErrNoSuchFile) {
		t.Errorf("err = %v, want ErrNoSuchFile for a file that has not changed", err)
	}
}

// The path comes from a URL and reaches "git diff --no-index", which reads any
// file on the disk it is pointed at.
func TestFileDiffRejectsPathsOutsideTheRepo(t *testing.T) {
	dir := repo(t)
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "", "a/../../b"} {
		if _, err := New("").FileDiff(context.Background(), dir, BaseHead, bad, ""); err == nil {
			t.Errorf("FileDiff(%q) was allowed", bad)
		}
	}
	if _, err := New("").FileDiff(context.Background(), dir, BaseHead, "keep.txt", "../elsewhere"); err == nil {
		t.Error("FileDiff with an escaping old path was allowed")
	}
}

func TestChangesOnPlainDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	_, err := New("").Changes(context.Background(), t.TempDir(), BaseHead)
	if !errors.Is(err, ErrNotRepo) {
		t.Errorf("err = %v, want ErrNotRepo", err)
	}
}

func TestParseBase(t *testing.T) {
	for in, want := range map[string]Base{"": BaseHead, "head": BaseHead, "branch": BaseBranch, "nonsense": BaseHead} {
		if got := ParseBase(in); got != want {
			t.Errorf("ParseBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUnifiedTracksLineNumbersAcrossHunks(t *testing.T) {
	diff := "diff --git a/f b/f\n" +
		"--- a/f\n+++ b/f\n" +
		"@@ -1,3 +1,3 @@ func main() {\n one\n-two\n+TWO\n three\n" +
		"@@ -20,2 +20,3 @@\n twenty\n+extra\n twentyone\n\\ No newline at end of file\n"

	hunks, binary, truncated := parseUnified([]byte(diff))
	if binary || truncated {
		t.Fatalf("binary = %v, truncated = %v, want both false", binary, truncated)
	}
	if len(hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(hunks))
	}
	if hunks[0].Header != "func main() {" {
		t.Errorf("header = %q, want the section name", hunks[0].Header)
	}
	if hunks[0].Range != "@@ -1,3 +1,3 @@" {
		t.Errorf("range = %q, want git's own header", hunks[0].Range)
	}
	second := hunks[1]
	if second.OldLine != 20 || second.NewLine != 20 {
		t.Errorf("second hunk starts at %d/%d, want 20/20", second.OldLine, second.NewLine)
	}
	last := second.Lines[len(second.Lines)-1]
	if last.Kind != "note" || last.Text != "No newline at end of file" {
		t.Errorf("last line = %+v, want git's own note", last)
	}
	// The added line pushes the following context line apart on the two sides.
	ctx := second.Lines[2]
	if ctx.Kind != "ctx" || ctx.Old != 21 || ctx.New != 22 {
		t.Errorf("trailing context = %+v, want ctx at 21/22", ctx)
	}
}

func TestParseNameStatusReadsRenames(t *testing.T) {
	data := []byte("M\x00keep.txt\x00R087\x00old.txt\x00new.txt\x00D\x00gone.txt\x00")
	got, err := parseNameStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "keep.txt", Status: "modified"},
		{Path: "new.txt", OldPath: "old.txt", Status: "renamed"},
		{Path: "gone.txt", Status: "deleted"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// numstat leaves the path empty for a rename and follows it with the two
// names, which is the only thing separating that record from an ordinary one.
func TestApplyNumstatHandlesRenamesAndBinaries(t *testing.T) {
	changes := []Change{
		{Path: "keep.txt", Status: "modified"},
		{Path: "new.txt", OldPath: "old.txt", Status: "renamed"},
		{Path: "blob.bin", Status: "modified"},
	}
	data := []byte("3\t1\tkeep.txt\x005\t2\t\x00old.txt\x00new.txt\x00-\t-\tblob.bin\x00")
	if err := applyNumstat(data, changes); err != nil {
		t.Fatal(err)
	}
	if changes[0].Added != 3 || changes[0].Removed != 1 {
		t.Errorf("keep.txt = %+v, want +3 -1", changes[0])
	}
	if changes[1].Added != 5 || changes[1].Removed != 2 {
		t.Errorf("new.txt = %+v, want +5 -2", changes[1])
	}
	if !changes[2].Binary || changes[2].Added != 0 {
		t.Errorf("blob.bin = %+v, want binary with no counts", changes[2])
	}
}

func TestCountLines(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "text.txt", "a\nb\nc\n")
	if n, binary := countLines(filepath.Join(dir, "text.txt")); n != 3 || binary {
		t.Errorf("text.txt = %d lines, binary %v, want 3 and false", n, binary)
	}
	write(t, dir, "blob.bin", "a\x00b\n")
	if n, binary := countLines(filepath.Join(dir, "blob.bin")); !binary || n != 0 {
		t.Errorf("blob.bin = %d lines, binary %v, want 0 and true", n, binary)
	}
	if n, binary := countLines(filepath.Join(dir, "missing")); n != 0 || binary {
		t.Errorf("missing file = %d lines, binary %v, want 0 and false", n, binary)
	}
}
