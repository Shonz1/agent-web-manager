package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingSbx is fakeSbx that also keeps a tally, so a test can say how many
// container round trips a read cost — which is the only thing that makes a
// sandbox diff slow, and the only thing these tests are about.
func countingSbx(t *testing.T, tally string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\n" +
		"echo 'Sandbox demo started successfully'\n" +
		"echo call >>'" + tally + "'\n" +
		"shift 2\nexec \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func calls(t *testing.T, tally string) int {
	t.Helper()
	data, err := os.ReadFile(tally)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(data)))
}

// Reading a change list used to take ten "sbx exec"s and opening one file from
// it another seven, all of them milliseconds of git behind a container round
// trip. What is left is one round trip for what the repository is, one for the
// list, one to count the untracked files in it — and, once the first of those
// is cached, one apiece for the reads that follow.
func TestASandboxReadCostsOneRoundTripPerThingRead(t *testing.T) {
	dir := repo(t)
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")
	write(t, dir, "fresh.txt", "brand new\nlines\n")

	tally := filepath.Join(t.TempDir(), "calls")
	c := New("git").InSandbox(countingSbx(t, tally), "demo")

	got, err := c.Changes(context.Background(), dir, BaseHead)
	if err != nil {
		t.Fatal(err)
	}
	// probe, the three lists, and the count of the one untracked file.
	if n := calls(t, tally); n != 3 {
		t.Errorf("first change list took %d round trips, want 3", n)
	}
	if c := changed(t, got, "fresh.txt"); c.Added != 2 {
		t.Errorf("fresh.txt = %+v, want +2 — the batched read lost its count", c)
	}

	// The view re-reads on a timer, and what the repository is has not moved.
	before := calls(t, tally)
	if _, err := c.Changes(context.Background(), dir, BaseHead); err != nil {
		t.Fatal(err)
	}
	if n := calls(t, tally) - before; n != 2 {
		t.Errorf("second change list took %d round trips, want 2 — the probe was not reused", n)
	}

	// Opening a file asks nothing the list already answered.
	for _, file := range []string{"keep.txt", "fresh.txt"} {
		before = calls(t, tally)
		diff, err := c.FileDiff(context.Background(), dir, BaseHead, file, "")
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(diff.Hunks) == 0 {
			t.Errorf("%s came back with no hunks", file)
		}
		if n := calls(t, tally) - before; n != 1 {
			t.Errorf("opening %s took %d round trips, want 1", file, n)
		}
	}
}

// The client is copied for every request — web builds one per call with
// InSandbox — so a cache that went with the copy would never be read twice.
func TestProbeIsSharedByEveryClientMadeFromOne(t *testing.T) {
	dir := repo(t)
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")

	tally := filepath.Join(t.TempDir(), "calls")
	base := New("git")
	sbx := countingSbx(t, tally)

	for i := range 3 {
		// A fresh client each time, exactly as a request gets one.
		if _, err := base.InSandbox(sbx, "demo").Changes(context.Background(), dir, BaseHead); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	// One probe for the first read, and a list for each of the three. No
	// untracked files here, so nothing to count.
	if n := calls(t, tally); n != 4 {
		t.Errorf("three reads took %d round trips, want 4", n)
	}
}

// The cache is reached from whichever request happens to be reading, and
// several of them are in flight at once — the change list polls while a file
// is being opened. Run under -race, this is what says that is safe.
func TestProbeIsReadFromSeveralRequestsAtOnce(t *testing.T) {
	dir := repo(t)
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")

	base := New("git")
	sbx := fakeSbx(t)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 8 {
		wg.Add(2)
		// Two sandboxes, so the readers are writing different keys as well as
		// racing on the same one.
		name := []string{"one", "two"}[i%2]
		go func() {
			defer wg.Done()
			_, err := base.InSandbox(sbx, name).Changes(context.Background(), dir, BaseHead)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := base.InSandbox(sbx, name).FileDiff(context.Background(), dir, BaseHead, "keep.txt", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Two sandboxes have the same workspace path and different checkouts in it,
// so one may not be told what the other found.
func TestProbeIsNotSharedBetweenSandboxes(t *testing.T) {
	dir := repo(t)
	tally := filepath.Join(t.TempDir(), "calls")
	base := New("git")
	sbx := countingSbx(t, tally)

	for _, name := range []string{"one", "two"} {
		if _, err := base.InSandbox(sbx, name).Changes(context.Background(), dir, BaseHead); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if n := calls(t, tally); n != 4 {
		t.Errorf("two sandboxes took %d round trips, want 4 — a probe crossed between them", n)
	}
}

// A branch that moves has to be noticed. The cache is a few seconds wide, not
// a promise that the answer is fixed.
func TestProbeIsForgottenOnceItIsOldEnough(t *testing.T) {
	dir := repo(t)
	tally := filepath.Join(t.TempDir(), "calls")
	c := New("git").InSandbox(countingSbx(t, tally), "demo")

	now := time.Now()
	c.probes.now = func() time.Time { return now }

	if _, err := c.Changes(context.Background(), dir, BaseHead); err != nil {
		t.Fatal(err)
	}
	before := calls(t, tally)

	now = now.Add(probeTTL)
	if _, err := c.Changes(context.Background(), dir, BaseHead); err != nil {
		t.Fatal(err)
	}
	if n := calls(t, tally) - before; n != 2 {
		t.Errorf("read after the entry aged out took %d round trips, want 2 (probe and list)", n)
	}
}

// There are two readings of the same repository now — one command at a time
// here, and a script's worth at a time in a sandbox — and the whole point is
// that they are the same reading. Nothing but a comment says so, which is why
// this compares them over every kind of change there is, on both bases.
func TestASandboxReadsWhatThisMachineWouldHaveRead(t *testing.T) {
	dir := repo(t)
	// A branch with a commit on it, so that comparing against the branch base
	// has a span of commits to find rather than falling back to HEAD.
	run(t, dir, "checkout", "-q", "-b", "feat")
	write(t, dir, "keep.txt", "one\nTWO\nthree\n")
	run(t, dir, "commit", "-qam", "committed on the branch")

	run(t, dir, "rm", "-q", "gone.txt")
	run(t, dir, "mv", "moved.txt", "renamed.txt")
	write(t, dir, "renamed.txt", "a\nb\nc\nd\ne\nf\ng\nH\n")
	write(t, dir, "fresh.txt", "brand new\nlines\n")
	write(t, dir, "blob.bin", "\x00\x01\n")

	here := New("git")
	there := New("git").InSandbox(fakeSbx(t), "demo")

	for _, base := range []Base{BaseHead, BaseBranch} {
		t.Run(string(base), func(t *testing.T) {
			want, err := here.Changes(context.Background(), dir, base)
			if err != nil {
				t.Fatal(err)
			}
			got, err := there.Changes(context.Background(), dir, base)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("in a sandbox:\n %+v\nhere:\n %+v", got, want)
			}
			if base == BaseBranch && want.BaseRef != "main" {
				t.Fatalf("baseRef = %q, want main — the branch base was not found, so this proved nothing", want.BaseRef)
			}

			for _, f := range want.Files {
				wantDiff, err := here.FileDiff(context.Background(), dir, base, f.Path, f.OldPath)
				if err != nil {
					t.Fatalf("%s here: %v", f.Path, err)
				}
				gotDiff, err := there.FileDiff(context.Background(), dir, base, f.Path, f.OldPath)
				if err != nil {
					t.Fatalf("%s in a sandbox: %v", f.Path, err)
				}
				if fmt.Sprint(gotDiff) != fmt.Sprint(wantDiff) {
					t.Errorf("%s in a sandbox:\n %+v\nhere:\n %+v", f.Path, gotDiff, wantDiff)
				}
			}
		})
	}
}

// The batched reads frame their sections by length rather than by a separator,
// because a path may hold any byte but NUL — a newline included, which is what
// a separator would have been.
func TestSectionsFramesContentItCannotEscape(t *testing.T) {
	awkward := "a\nb\x00c \x00" // newline, NUL, and a space
	stream := fmt.Sprintf("one %d\n%stwo 0\n", len(awkward), awkward)

	got, err := sections([]byte(stream), false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["one"]) != awkward {
		t.Errorf("one = %q, want %q", got["one"], awkward)
	}
	if _, ok := got["two"]; !ok {
		t.Error("an empty section is still a section that was read")
	}
}

// wc pads its count on some systems, and the framing is read on this machine
// rather than the one that wrote it.
func TestSectionsAcceptsAPaddedLength(t *testing.T) {
	got, err := sections([]byte("root      3\nabc"), false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["root"]) != "abc" {
		t.Errorf("root = %q, want abc", got["root"])
	}
}

// A stream cut short at maxOutput is a diff too big to show whole. That is
// something to report, not a reason to fail to read what did arrive.
func TestSectionsKeepsWhatArrivedOfATruncatedStream(t *testing.T) {
	if _, err := sections([]byte("one 99\nshort"), false); err == nil {
		t.Error("a short section with nothing cut is a stream that does not parse")
	}
	got, err := sections([]byte("one 99\nshort"), true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["one"]) != "short" {
		t.Errorf("one = %q, want what arrived of it", got["one"])
	}
}

func TestSectionsRejectsAStreamThatIsNotOne(t *testing.T) {
	for _, bad := range []string{"no-length\nbody", "one x\nbody", "one -1\nbody"} {
		if _, err := sections([]byte(bad), false); err == nil {
			t.Errorf("sections(%q) = no error, want one", bad)
		}
	}
}

// baseFor is the whole of what resolving a base now costs: probe read the
// facts, and this decides what to do with them.
func TestBaseForPicksTheRevisionToCompareAgainst(t *testing.T) {
	tests := []struct {
		name     string
		info     repoInfo
		base     Base
		rev, ref string
	}{
		{
			name: "no commits yet, so everything reads as added",
			info: repoInfo{},
			base: BaseHead,
			rev:  emptyTree, ref: "the empty tree",
		},
		{
			name: "uncommitted work is measured from HEAD",
			info: repoInfo{Head: "abc", Default: "main", MergeBase: "def"},
			base: BaseHead,
			rev:  "HEAD", ref: "HEAD",
		},
		{
			name: "a branch is measured from where it left the default",
			info: repoInfo{Head: "abc", Branch: "feat", Default: "main", MergeBase: "def"},
			base: BaseBranch,
			rev:  "def", ref: "main",
		},
		{
			name: "a branch that has not left the default yet has no span to show",
			info: repoInfo{Head: "abc", Default: "main", MergeBase: "abc"},
			base: BaseBranch,
			rev:  "HEAD", ref: "HEAD",
		},
		{
			name: "nothing to have branched from",
			info: repoInfo{Head: "abc"},
			base: BaseBranch,
			rev:  "HEAD", ref: "HEAD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rev, ref := baseFor(tt.info, tt.base)
			if rev != tt.rev || ref != tt.ref {
				t.Errorf("baseFor = %q/%q, want %q/%q", rev, ref, tt.rev, tt.ref)
			}
		})
	}
}
