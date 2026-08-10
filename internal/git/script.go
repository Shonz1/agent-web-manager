package git

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// The scripts here each stand in for a handful of git commands.
//
// On this machine that would be pointless: git answers any of these in
// milliseconds, and running them one at a time is clearer. Inside a sandbox
// every command is an "sbx exec" — a container round trip that costs orders of
// magnitude more than the git it carries — so what matters there is not how
// much git is asked to do but how many times it is asked. Reading a change
// list took ten of those round trips, and opening one file from it another
// seven; these bring both down to one.
//
// Each script that has more than one thing to say frames its sections by
// length: a line naming the section and its size in bytes, then exactly that
// many bytes. A separator would have been simpler and wrong — these carry
// NUL-separated lists of paths, and a path may contain any byte but NUL,
// newlines included.

// probeScript reads what a repository is: where its top is, what is on HEAD,
// what is checked out, and what this branch appears to have grown out of.
//
// Only the first command decides whether this succeeded. The rest are allowed
// to fail and come back empty, because a repository with no commits or no
// remote is an ordinary thing to be looking at rather than a failure to read.
const probeScript = `set -u
dir=$1
d=$(mktemp -d) || exit 1
trap 'rm -rf "$d"' EXIT

git -C "$dir" rev-parse --show-toplevel >"$d/root" || exit $?
root=$(cat "$d/root")

: >"$d/head"; : >"$d/branch"; : >"$d/default"; : >"$d/mergebase"
git -C "$root" rev-parse --verify --quiet "HEAD^{commit}" >"$d/head" 2>/dev/null || :
git -C "$root" rev-parse --abbrev-ref HEAD >"$d/branch" 2>/dev/null || :
git -C "$root" symbolic-ref --quiet --short refs/remotes/origin/HEAD >"$d/default" 2>/dev/null || :
if [ ! -s "$d/default" ]; then
	for n in origin/main origin/master main master; do
		if git -C "$root" rev-parse --verify --quiet "$n^{commit}" >/dev/null 2>&1; then
			printf '%s' "$n" >"$d/default"
			break
		fi
	done
fi
if [ -s "$d/default" ] && [ -s "$d/head" ]; then
	git -C "$root" merge-base "$(cat "$d/default")" HEAD >"$d/mergebase" 2>/dev/null || :
fi

for s in root head branch default mergebase; do
	printf '%s %s\n' "$s" "$(wc -c <"$d/$s")"
	cat "$d/$s"
done`

// listScript reads the three lists a change list is built from: what differs
// from the base, how much of each differs, and what git has never been told
// about. Any of them failing fails the lot — an incomplete change list is
// worse than none, since nothing about it would say what is missing.
const listScript = `set -u
root=$1
rev=$2
d=$(mktemp -d) || exit 1
trap 'rm -rf "$d"' EXIT

git -C "$root" diff --name-status -z --no-color "$rev" -- >"$d/status" || exit $?
git -C "$root" diff --numstat -z --no-color "$rev" -- >"$d/numstat" || exit $?
git -C "$root" ls-files --others --exclude-standard -z >"$d/others" || exit $?

for s in status numstat others; do
	printf '%s %s\n' "$s" "$(wc -c <"$d/$s")"
	cat "$d/$s"
done`

// fileScript reads one file's diff, working out first which kind of file it is.
//
// An untracked file has nothing in the index to compare against, so it is
// diffed against nothing at all: --no-index is the one form of git diff that
// will do that, and it reports what it found through its exit status — 1 means
// the two differ, which is precisely what it was asked. Only that one status is
// swallowed, so a real failure still comes back as one.
const fileScript = `set -u
root=$1
rev=$2
file=$3
old=$4

if [ -n "$(git -C "$root" ls-files --others --exclude-standard -- "$file")" ]; then
	git -C "$root" diff --no-index --no-color -- /dev/null "$file" || [ $? -eq 1 ]
elif [ -n "$old" ]; then
	git -C "$root" diff --no-color --find-renames "$rev" -- "$old" "$file"
else
	git -C "$root" diff --no-color "$rev" -- "$file"
fi`

// sections splits the length-framed stream the scripts above write.
//
// truncated says the stream was cut short at maxOutput, in which case the last
// section is however much of it arrived: that is a diff too big to show whole,
// which is a thing to report rather than an unreadable answer.
func sections(data []byte, truncated bool) (map[string][]byte, error) {
	out := make(map[string][]byte)
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			if truncated {
				return out, nil
			}
			return nil, fmt.Errorf("unterminated section header %q", head(data))
		}
		header := string(data[:nl])
		data = data[nl+1:]

		name, size, ok := strings.Cut(header, " ")
		if !ok || name == "" {
			return nil, fmt.Errorf("malformed section header %q", header)
		}
		n, err := strconv.Atoi(strings.TrimSpace(size))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("malformed section length in %q", header)
		}
		if n > len(data) {
			if truncated {
				out[name] = data
				return out, nil
			}
			return nil, fmt.Errorf("section %q wants %d bytes and has %d", name, n, len(data))
		}
		out[name] = data[:n]
		data = data[n:]
	}
	return out, nil
}

// head is enough of a malformed stream to name it in an error without pasting
// a diff into one.
func head(data []byte) []byte {
	if len(data) > 80 {
		return data[:80]
	}
	return data
}
