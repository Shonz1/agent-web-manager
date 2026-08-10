package manager

import (
	"testing"
	"time"
)

// notified reports whether a watcher has been woken, without waiting: every
// change here is made synchronously before the check.
func notified(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestWatchFollowsShellCommands(t *testing.T) {
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	changed, unwatch := s.Watch()
	defer unwatch()

	s.noteInput([]byte("git status\r"))
	if !notified(changed) {
		t.Fatal("a new command did not wake the watcher")
	}

	// Half a line is not something anyone can see yet.
	s.noteInput([]byte("git lo"))
	if notified(changed) {
		t.Fatal("an unfinished line woke the watcher")
	}

	// Running the same command again leaves nothing to redraw.
	s.noteInput([]byte("g\x03git status\r"))
	if notified(changed) {
		t.Fatal("an unchanged command woke the watcher")
	}

	unwatch()
	s.noteInput([]byte("pwd\r"))
	if notified(changed) {
		t.Fatal("a stopped watcher was still woken")
	}
}

func TestWatchFollowsAgentTitles(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")
	changed, unwatch := s.Watch()
	defer unwatch()

	s.setAITitle("Create schema-based query service")
	if !notified(changed) {
		t.Fatal("a new title did not wake the watcher")
	}

	s.setAITitle("Create schema-based query service")
	if notified(changed) {
		t.Fatal("an unchanged title woke the watcher")
	}
}

// A watcher that is not keeping up must not stall whoever is typing, and the
// notification it does get is enough: the view is read afresh.
func TestWatchDoesNotBlockOnASlowWatcher(t *testing.T) {
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	changed, unwatch := s.Watch()
	defer unwatch()

	for _, cmd := range []string{"one\r", "two\r", "three\r"} {
		s.noteInput([]byte(cmd))
	}
	if !notified(changed) {
		t.Fatal("watcher was not woken")
	}
	if got := s.View().LastCommand; got != "three" {
		t.Fatalf("last command %q, want %q", got, "three")
	}
}

// The order sessions are listed in follows the person, not the agent: an agent
// drawing to the screen, and the activity that is read off it, must leave a
// session's place alone. Only a keystroke moves it.
func TestLastActivityFollowsUserInputNotAgentOutput(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")
	uses := 0
	s.onUserInput = func(*Session) { uses++ }

	started := s.View().LastActivityAt
	if started.IsZero() {
		t.Fatal("a session that has just been started has been used once, not never")
	}

	s.noteOutput()
	s.setActivity(ActivityBusy)
	s.setActivity(ActivityIdle)
	if got := s.View().LastActivityAt; !got.Equal(started) {
		t.Errorf("the agent working moved LastActivityAt from %v to %v", started, got)
	}
	if uses != 0 {
		t.Errorf("the agent working reported %d uses, want 0", uses)
	}

	time.Sleep(time.Millisecond)
	s.noteUsed()
	if got := s.View().LastActivityAt; !got.After(started) {
		t.Errorf("LastActivityAt = %v, want after %v", got, started)
	}
	if uses != 1 {
		t.Errorf("typing reported %d uses, want 1", uses)
	}
}

// The same thing through the whole path: a real process drawing to a real PTY
// on its own is not someone using the session, and writing to it is.
func TestRealSessionOutputIsNotUse(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell for a second")
	}
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	uses := 0
	s.onUserInput = func(*Session) { uses++ }

	env := []string{"PS1=$ ", "TERM=xterm-256color", "PATH=/usr/bin:/bin"}
	// Prints on its own, then waits: output nobody asked for, and a process
	// still alive to be written to afterwards.
	if err := s.start("/bin/sh", []string{"-c", "echo working; sleep 30"}, env, 80, 24); err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(s.terminate)

	started := s.View().LastActivityAt
	if uses != 1 {
		t.Fatalf("starting the session reported %d uses, want 1", uses)
	}

	time.Sleep(500 * time.Millisecond)
	if got := s.View().LastActivityAt; !got.Equal(started) {
		t.Errorf("output the shell produced on its own moved LastActivityAt from %v to %v", started, got)
	}

	if err := s.Write([]byte("\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := s.View().LastActivityAt; !got.After(started) {
		t.Errorf("LastActivityAt = %v, want after %v", got, started)
	}
	if uses != 2 {
		t.Errorf("typing reported %d uses in total, want 2", uses)
	}
}
