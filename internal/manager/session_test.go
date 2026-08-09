package manager

import "testing"

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
