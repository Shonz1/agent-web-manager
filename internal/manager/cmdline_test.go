package manager

import (
	"strings"
	"testing"
)

func TestCmdLineFeed(t *testing.T) {
	tests := []struct {
		name  string
		keys  []string
		want  string
		after string // what a following Enter submits, i.e. what is left on the line
	}{
		{
			name: "plain command",
			keys: []string{"go test ./...\r"},
			want: "go test ./...",
		},
		{
			name: "typed one keystroke at a time",
			keys: []string{"l", "s", " ", "-", "l", "\r"},
			want: "ls -l",
		},
		{
			name: "newline submits too",
			keys: []string{"pwd\n"},
			want: "pwd",
		},
		{
			name: "surrounding space is dropped",
			keys: []string{"   make build   \r"},
			want: "make build",
		},
		{
			name: "an empty line submits nothing",
			keys: []string{"\r", "   \r"},
			want: "",
		},
		{
			name: "backspace erases",
			keys: []string{"gitt\x7f status\r"},
			want: "git status",
		},
		{
			name: "ctrl-w erases the last word",
			keys: []string{"git status \x17log\r"},
			want: "git log",
		},
		{
			name: "ctrl-c abandons the line",
			keys: []string{"rm -rf /\x03", "ls\r"},
			want: "ls",
		},
		{
			name: "ctrl-u abandons the line",
			keys: []string{"rm -rf /\x15ls\r"},
			want: "ls",
		},
		{
			name: "arrow keys are not text",
			keys: []string{"ec\x1b[Dho hi\r"},
			want: "echo hi",
		},
		{
			name: "an escape sequence split across writes is still skipped",
			keys: []string{"echo\x1b", "[A", " hi\r"},
			want: "echo hi",
		},
		{
			name: "alt-key sequences are not text",
			keys: []string{"echo\x1bb hi\r"},
			want: "echo hi",
		},
		{
			name: "the last of several commands wins",
			keys: []string{"cd /tmp\rls\rpwd\r"},
			want: "pwd",
		},
		{
			name: "non-ascii survives",
			keys: []string{"echo héllo\r"},
			want: "echo héllo",
		},
		{
			name: "backspace over a multi-byte rune erases the whole rune",
			keys: []string{"echo é\x7fo\r"},
			want: "echo o",
		},
		{
			name:  "an unfinished line is not submitted",
			keys:  []string{"git comm"},
			want:  "",
			after: "git comm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c cmdLine
			var got string
			for _, k := range tt.keys {
				if cmd := c.feed([]byte(k)); cmd != "" {
					got = cmd
				}
			}
			if got != tt.want {
				t.Errorf("submitted %q, want %q", got, tt.want)
			}
			if tt.after != "" {
				if left := c.feed([]byte("\r")); left != tt.after {
					t.Errorf("line held %q, want %q", left, tt.after)
				}
			}
		})
	}
}

func TestCmdLineBounded(t *testing.T) {
	var c cmdLine
	c.feed([]byte(strings.Repeat("x", maxCmdLine*2)))
	if len(c.buf) != maxCmdLine {
		t.Fatalf("line grew to %d runes, want it capped at %d", len(c.buf), maxCmdLine)
	}
}

func TestCmdLineReset(t *testing.T) {
	var c cmdLine
	c.feed([]byte("half a command\x1b["))
	c.reset()
	if cmd := c.feed([]byte("ls\r")); cmd != "ls" {
		t.Fatalf("after reset submitted %q, want %q", cmd, "ls")
	}
}

// A shell session's last command stands in for the title an agent gives
// itself, so it goes through the same trimming.
func TestNoteInputTracksShellCommands(t *testing.T) {
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	s.noteInput([]byte("git status\r"))
	if got := s.View().LastCommand; got != "git status" {
		t.Fatalf("last command %q, want %q", got, "git status")
	}

	long := "echo " + strings.Repeat("word ", 40)
	s.noteInput([]byte(long + "\r"))
	if got := s.View().LastCommand; len([]rune(got)) != maxCommandTitle {
		t.Fatalf("long command was not trimmed to %d runes: %q", maxCommandTitle, got)
	}
}

// The line is followed on the way to the PTY, so it is there whether or not
// anyone is attached to the session.
func TestWriteTracksCommand(t *testing.T) {
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	if err := s.start("cat", nil, nil, 80, 24); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.terminate()

	if err := s.Write([]byte("go build ./...\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := s.View().LastCommand; got != "go build ./..." {
		t.Fatalf("last command %q, want %q", got, "go build ./...")
	}
}

// An agent's keystrokes are answers to what it asked, not commands.
func TestNoteInputIgnoresAgentSessions(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")
	s.noteInput([]byte("write me a haiku\r"))
	if got := s.View().LastCommand; got != "" {
		t.Fatalf("agent session recorded a command: %q", got)
	}
}
