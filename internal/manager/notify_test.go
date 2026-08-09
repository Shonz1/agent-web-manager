package manager

import (
	"testing"
	"time"
)

// The dwells under test are scaled right down: what is being checked is the
// shape of the state machine, not the wall-clock values chosen for it.
var testDwell = dwell{
	attention: 20 * time.Millisecond,
	done:      20 * time.Millisecond,
	minBusy:   30 * time.Millisecond,
	reask:     30 * time.Millisecond,
}

// collector gathers what a notifier emitted.
type collector struct {
	ch chan Event
}

func newCollector() *collector {
	return &collector{ch: make(chan Event, 8)}
}

func (c *collector) emit(ev Event) { c.ch <- ev }

// waitFor returns the next event, or fails if none arrives. The wait is far
// longer than any dwell above so that a loaded machine does not fail the test.
func (c *collector) waitFor(t *testing.T) Event {
	t.Helper()
	return c.waitWithin(t, 2*time.Second)
}

func (c *collector) waitWithin(t *testing.T, d time.Duration) Event {
	t.Helper()
	select {
	case ev := <-c.ch:
		return ev
	case <-time.After(d):
		t.Fatal("expected an event, got none")
		return Event{}
	}
}

// waitQuiet fails if anything is emitted, having given a dwell time to elapse
// several times over.
func (c *collector) waitQuiet(t *testing.T) {
	t.Helper()
	select {
	case ev := <-c.ch:
		t.Fatalf("expected no event, got %s for %q", ev.Kind, ev.Title)
	case <-time.After(200 * time.Millisecond):
	}
}

func newTestNotifier() (*Session, *sessionNotifier, *collector) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")
	c := newCollector()
	n := newSessionNotifier(c.emit, testDwell)
	s.onActivity = n.activityChanged
	return s, n, c
}

// worked puts the session through a run of work long enough to count, and
// leaves it busy.
func worked(n *sessionNotifier, s *Session) {
	n.activityChanged(s, "", ActivityBusy)
	time.Sleep(testDwell.minBusy + 10*time.Millisecond)
}

func TestWaitingRaisesAttention(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, ActivityBusy, ActivityWaiting)

	ev := c.waitFor(t)
	if ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}
	if ev.SessionID != "s1" || ev.Title != "claude" || ev.SandboxName != "box" {
		t.Fatalf("event does not identify the session: %+v", ev)
	}
}

func TestFinishingRealWorkReportsDone(t *testing.T) {
	s, n, c := newTestNotifier()

	worked(n, s)
	n.activityChanged(s, ActivityBusy, ActivityIdle)

	if ev := c.waitFor(t); ev.Kind != EventDone {
		t.Fatalf("kind %q, want %q", ev.Kind, EventDone)
	}
}

// Every session starts out busy and a shell reaches its prompt in under a
// second. Announcing that as a completed task is the noise this guards.
func TestSettlingAtAPromptIsNotDone(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, "", ActivityBusy)
	n.activityChanged(s, ActivityBusy, ActivityIdle)

	c.waitQuiet(t)
}

// An agent that pauses mid-turn — a long tool call, a spinner this manager
// does not recognise — reads idle for a moment. The dwell is what keeps that
// from being reported as an ending.
func TestAPauseMidTurnIsNotDone(t *testing.T) {
	s, n, c := newTestNotifier()

	worked(n, s)
	n.activityChanged(s, ActivityBusy, ActivityIdle)
	// Back to work before the dwell is up.
	n.activityChanged(s, ActivityIdle, ActivityBusy)

	c.waitQuiet(t)
}

// The same for a question that was drawn, redrawn, and gone again before
// anybody could have answered it.
func TestAFlickeredQuestionRaisesNothing(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	n.activityChanged(s, ActivityWaiting, ActivityBusy)

	c.waitQuiet(t)
}

// A question that is still up is redrawn, and a repaint is output like any
// other — so activity leaves waiting and comes straight back. It is the same
// question, and it was announced the first time round.
func TestARedrawnQuestionIsAnnouncedOnce(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}

	// Several repaints, each a flicker of work too short to be any.
	for i := 0; i < 3; i++ {
		n.activityChanged(s, ActivityWaiting, ActivityBusy)
		n.activityChanged(s, ActivityBusy, ActivityWaiting)
	}

	c.waitQuiet(t)
}

// The other half of that: a question answered, work done, and the agent asking
// again is two questions, and has to be told twice.
func TestAQuestionAfterRealWorkIsAnnouncedAgain(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("first question: kind %q, want %q", ev.Kind, EventAttention)
	}

	// Answered, then a real stretch of work, then asked again.
	n.activityChanged(s, ActivityWaiting, ActivityBusy)
	time.Sleep(testDwell.reask + 10*time.Millisecond)
	n.activityChanged(s, ActivityBusy, ActivityWaiting)

	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("second question: kind %q, want %q", ev.Kind, EventAttention)
	}
}

// Going quiet at a prompt means whatever was being asked is behind us, so the
// next question starts over regardless of how much work came in between.
func TestGoingIdleClearsTheAnnouncedQuestion(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}

	n.activityChanged(s, ActivityWaiting, ActivityIdle)
	n.activityChanged(s, ActivityIdle, ActivityWaiting)

	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}
}

// A question that came and went inside its own dwell was never announced, so
// it must not be what silences the one that follows.
func TestAQuestionTooBriefToAnnounceDoesNotSilenceTheNext(t *testing.T) {
	s, n, c := newTestNotifier()

	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	n.activityChanged(s, ActivityWaiting, ActivityBusy) // gone before the dwell
	c.waitQuiet(t)

	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}
}

// A session whose process is gone has not finished a task, and anything
// pending about it describes something that no longer exists.
func TestAnExitWithdrawsAPendingEvent(t *testing.T) {
	s, n, c := newTestNotifier()

	worked(n, s)
	n.activityChanged(s, ActivityBusy, ActivityIdle)
	n.activityChanged(s, ActivityIdle, "")

	c.waitQuiet(t)
}

// Answering a question and going quiet again is not the end of a stretch of
// work: the clock starts when the work does, and waiting is not working.
func TestWorkIsNotCreditedAcrossAQuestion(t *testing.T) {
	s, n, c := newTestNotifier()

	worked(n, s)
	n.activityChanged(s, ActivityBusy, ActivityWaiting)
	if ev := c.waitFor(t); ev.Kind != EventAttention {
		t.Fatalf("kind %q, want %q", ev.Kind, EventAttention)
	}

	// Answered, a moment's work, then quiet — too little to be an ending.
	n.activityChanged(s, ActivityWaiting, ActivityBusy)
	n.activityChanged(s, ActivityBusy, ActivityIdle)

	c.waitQuiet(t)
}

// A restart must not inherit the previous run's clock, or a session would
// report finishing work that a process which no longer exists did.
func TestARestartStartsTheClockOver(t *testing.T) {
	s, n, c := newTestNotifier()

	worked(n, s)
	n.activityChanged(s, ActivityBusy, "") // the process exits

	// start() says so for a run it set up itself.
	n.activityChanged(s, "", ActivityBusy)
	n.activityChanged(s, ActivityBusy, ActivityIdle)

	c.waitQuiet(t)
}

// What an agent called its own conversation is the most useful thing a
// notification can carry, so it rides along.
func TestEventCarriesWhatTheSessionIsWorkingOn(t *testing.T) {
	s, n, c := newTestNotifier()
	s.setAITitle("Add a rate limiter to the API")

	n.activityChanged(s, ActivityBusy, ActivityWaiting)

	if got := c.waitFor(t).Detail; got != "Add a rate limiter to the API" {
		t.Fatalf("detail %q, want the agent's own title", got)
	}
}

func TestShellEventsCarryTheLastCommand(t *testing.T) {
	s := newSession("s2", "b1", "box", KindShell, nil, "shell")
	c := newCollector()
	n := newSessionNotifier(c.emit, testDwell)
	s.onActivity = n.activityChanged

	s.noteInput([]byte("make build\r"))
	worked(n, s)
	n.activityChanged(s, ActivityBusy, ActivityIdle)

	ev := c.waitFor(t)
	if ev.Kind != EventDone {
		t.Fatalf("kind %q, want %q", ev.Kind, EventDone)
	}
	if ev.Detail != "make build" {
		t.Fatalf("detail %q, want %q", ev.Detail, "make build")
	}
}

func TestEventsReachEverySubscriber(t *testing.T) {
	m := &Manager{eventSubs: make(map[int]chan Event)}

	first, stopFirst := m.Events()
	defer stopFirst()
	second, stopSecond := m.Events()

	m.emit(Event{Kind: EventAttention, SessionID: "s1"})

	for i, ch := range []<-chan Event{first, second} {
		select {
		case ev := <-ch:
			if ev.SessionID != "s1" {
				t.Fatalf("subscriber %d got %+v", i, ev)
			}
		default:
			t.Fatalf("subscriber %d got nothing", i)
		}
	}

	// A subscriber that has stopped is not written to at all, so its buffer
	// cannot fill and stall the emitter.
	stopSecond()
	m.emit(Event{Kind: EventDone, SessionID: "s2"})
	if len(second) != 0 {
		t.Fatal("a stopped subscriber was still sent to")
	}
	if len(first) != 1 {
		t.Fatalf("live subscriber has %d events, want 1", len(first))
	}
}
