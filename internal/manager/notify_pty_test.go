package manager

import (
	"testing"
	"time"
)

// The tests in notify_test.go drive the state machine directly, which says
// nothing about whether anything ever drives it. These run a real shell under
// a real PTY and check that what comes back off its screen reaches a
// notification — the whole path, from bytes to event.
//
// The dwells are shortened but the sampler and the quiet window are not: those
// belong to the thing under test, and they are why these take seconds.

// ptyDwell keeps minBusy above the ~1.2s of silence the classifier needs
// before it will believe a screen, so that "worked long enough" still means
// something, and keeps the rest short.
var ptyDwell = dwell{
	attention: 50 * time.Millisecond,
	done:      50 * time.Millisecond,
	minBusy:   2 * time.Second,
	// Kept at its real value: what this has to tell apart is a repaint from a
	// stretch of work, and the sizes of those are set by the classifier rather
	// than by the test.
	reask: 5 * time.Second,
}

func startShell(t *testing.T, c *collector, d dwell) *Session {
	t.Helper()
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	s.onActivity = newSessionNotifier(c.emit, d).activityChanged

	// A plain prompt: no theme, no title-setting, nothing that would put a
	// widget on the screen for the classifier to read.
	env := []string{"PS1=$ ", "TERM=xterm-256color", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if err := s.start("/bin/sh", []string{"-i"}, env, 80, 24); err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(s.terminate)
	return s
}

// A command that prints steadily reads as work, and the prompt it returns to
// reads as the end of it.
func TestRealShellReportsFinishedWork(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell for several seconds")
	}
	c := newCollector()
	s := startShell(t, c, ptyDwell)

	// Four seconds of output, comfortably past minBusy, then back to a prompt.
	if err := s.Write([]byte("i=0; while [ $i -lt 40 ]; do echo $i; sleep 0.1; i=$((i+1)); done\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Four seconds of work, then the second and a bit of silence the
	// classifier needs before it will read the prompt, then the dwell.
	ev := c.waitWithin(t, 15*time.Second)
	if ev.Kind != EventDone {
		t.Fatalf("kind %q, want %q", ev.Kind, EventDone)
	}
	if ev.SessionID != "s1" {
		t.Fatalf("event names session %q, want s1", ev.SessionID)
	}
	// A shell says what it is doing by what it was told to do.
	if ev.Detail == "" {
		t.Fatal("event carries no command")
	}
}

// A shell that asks something and waits is the "action required" case, and the
// one the whole feature exists for.
func TestRealShellReportsAQuestion(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell for several seconds")
	}
	c := newCollector()
	s := startShell(t, c, ptyDwell)

	if err := s.Write([]byte("printf 'Delete everything? (y/n) '; read answer\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	ev := c.waitFor(t)
	if ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}
}

// Opening a terminal and leaving it alone is the commonest thing that happens
// to one, and it must produce nothing at all.
func TestRealShellSittingIdleSaysNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell for several seconds")
	}
	c := newCollector()
	startShell(t, c, ptyDwell)

	// Well past the quiet window, and past the dwell that would follow it.
	select {
	case ev := <-c.ch:
		t.Fatalf("an untouched shell reported %s", ev.Kind)
	case <-time.After(3 * time.Second):
	}
}

// A question that sits there being redrawn is the shape that produced repeated
// Telegram messages for claude: every repaint reads as work, so activity
// leaves waiting and comes back, and each return used to arm a fresh
// notification. One question is one notification, however often it is drawn.
func TestRealShellRedrawingAQuestionNotifiesOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell for several seconds")
	}
	c := newCollector()
	s := startShell(t, c, ptyDwell)

	// Redrawn every two seconds, which is longer than the silence the
	// classifier waits for — so it really does oscillate rather than simply
	// reading busy throughout.
	const script = `printf 'Apply these changes? (y/n) '; ` +
		`i=0; while [ $i -lt 6 ]; do sleep 2; printf '\rApply these changes? (y/n) '; i=$((i+1)); done; ` +
		`read answer` + "\n"
	if err := s.Write([]byte(script)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if ev := c.waitWithin(t, 10*time.Second); ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}

	// The rest of the redraws must produce nothing at all.
	select {
	case ev := <-c.ch:
		t.Fatalf("the same question was announced again (%s)", ev.Kind)
	case <-time.After(8 * time.Second):
	}
}
