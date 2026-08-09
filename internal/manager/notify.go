package manager

import (
	"sync"
	"time"
)

// A session's activity (see activity.go) says what it looks like to be doing
// at this moment, and it is the right signal for a dot in a sidebar: sampled
// four times a second, wrong for a moment now and then, corrected by the next
// sample.
//
// It is the wrong signal to interrupt someone with. An agent repaints while it
// thinks, so a single turn flickers between busy and idle several times before
// it really ends, and every one of those flickers would be a notification.
// What follows turns that signal into the two moments a person actually wants
// pushed at them, and holds each one long enough to be sure it is not a
// flicker.
type EventKind string

const (
	// EventAttention is a session that has stopped on something only the
	// person can answer.
	EventAttention EventKind = "attention"
	// EventDone is a session that has finished a stretch of work and gone
	// quiet.
	EventDone EventKind = "done"
)

// Event is one moment in a session's life worth telling someone about.
type Event struct {
	Kind        EventKind `json:"kind"`
	SessionID   string    `json:"sessionId"`
	SandboxID   string    `json:"sandboxId"`
	SandboxName string    `json:"sandboxName"`
	Title       string    `json:"title"`
	// Detail is what the session is working on, when it has said: the name an
	// agent gave its own conversation, or the last command a shell ran.
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// dwell is how long each state has to hold before it is believed enough to
// interrupt someone with.
type dwell struct {
	attention time.Duration
	done      time.Duration
	minBusy   time.Duration
	reask     time.Duration
}

var defaultDwell = dwell{
	// A question is the more urgent of the two and by far the better founded:
	// it is matched against widgets the agent draws, not inferred from
	// silence. This is long enough only to ride out a menu being redrawn.
	attention: 3 * time.Second,
	// Silence is the weak one. An agent part-way through a long tool call
	// draws nothing either, and only the spinners this manager recognises tell
	// that apart from a finished turn — so an agent it does not recognise
	// needs the wait to avoid announcing a pause as an ending.
	done: 10 * time.Second,
	// How long a session must have been working before going quiet counts as
	// having finished something.
	//
	// This is what keeps the ordinary business of a terminal quiet. Every
	// session starts out marked busy and a shell reaches its prompt in under a
	// second, so without this, opening one would announce that it had
	// completed a task — and so would every `ls`.
	minBusy: 20 * time.Second,
	// How much work has to happen between two questions before the second one
	// counts as a question of its own.
	//
	// A question that is still up is not a still picture. The agent repaints
	// the box it is drawn in, and a repaint is output like any other, so
	// activity leaves waiting and comes back a moment later — one question,
	// announced over and over, which is what this stops. codex avoids the
	// whole business by saying "action required" in the terminal title, which
	// is believed ahead of the screen and so never wavers; claude has no such
	// title and oscillates.
	//
	// A repaint's worth of "work" is bounded by the silence the classifier
	// waits for, so anything under a second and a half is one of those. This
	// is comfortably above that and still short enough for the real case it
	// has to let through: an approval granted, a few seconds of work, and the
	// next approval asked for.
	reask: 5 * time.Second,
}

// sessionNotifier watches one session's activity and decides which of its
// changes are worth an event.
//
// It is driven entirely by transitions: every change cancels whatever was
// pending and, if the new state is one worth reporting, arms a timer for it.
// A state that survives its timer is one that held for the whole dwell, which
// is the thing being tested.
type sessionNotifier struct {
	emit  func(Event)
	dwell dwell

	mu        sync.Mutex
	busySince time.Time
	timer     *time.Timer
	// asked records that a question has been announced and not yet been left
	// behind, so that the same one being redrawn is not announced again.
	asked bool
	// seq counts state changes. A timer captures the value it was armed at and
	// stays silent if anything has moved since, which is what settles a change
	// racing a timer that is already on its way to firing.
	seq int
}

func newSessionNotifier(emit func(Event), d dwell) *sessionNotifier {
	return &sessionNotifier{emit: emit, dwell: d}
}

// activityChanged is called on every activity transition, after the session
// has released its own lock. A next of "" is a session whose process is gone.
func (n *sessionNotifier) activityChanged(s *Session, prev, next Activity) {
	n.mu.Lock()

	n.seq++
	seq := n.seq
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}

	var kind EventKind
	var delay time.Duration

	switch next {
	case ActivityBusy:
		// Work has started, or restarted after a question was answered.
		// Whatever was pending described a session that has moved on.
		if prev != ActivityBusy {
			n.busySince = time.Now()
		}
	case ActivityWaiting:
		// The same question redrawn is not a new question. Only a real stretch
		// of work in between says the last one was dealt with and this is the
		// agent asking again.
		if !n.asked || n.workedForLocked(n.dwell.reask) {
			kind, delay = EventAttention, n.dwell.attention
		}
	case ActivityIdle:
		// Only the end of a real stretch of work is an ending. Falling quiet
		// from anything else — a question that went away, a session that never
		// got going — is just a terminal sitting there.
		if prev == ActivityBusy && n.workedForLocked(n.dwell.minBusy) {
			kind, delay = EventDone, n.dwell.done
		}
		// Sitting at a prompt with nothing to do means whatever was being
		// asked is behind us, so the next question is a fresh one.
		n.asked = false
		n.busySince = time.Time{}
	default:
		n.asked = false
		n.busySince = time.Time{}
	}

	if kind == "" {
		n.mu.Unlock()
		return
	}
	n.timer = time.AfterFunc(delay, func() { n.fire(s, kind, seq) })
	n.mu.Unlock()
}

// workedForLocked reports whether the run of work that just ended lasted at
// least d. The caller holds n.mu.
func (n *sessionNotifier) workedForLocked(d time.Duration) bool {
	return !n.busySince.IsZero() && time.Since(n.busySince) >= d
}

// fire emits the event its timer was armed for, unless the session has moved
// on in the meantime.
func (n *sessionNotifier) fire(s *Session, kind EventKind, seq int) {
	n.mu.Lock()
	stale := seq != n.seq
	if !stale {
		// Left alone when stale: the timer field belongs to a newer arming,
		// and clearing it here would lose the handle that cancels it.
		n.timer = nil
		if kind == EventAttention {
			// Recorded here rather than when the timer was armed: a question
			// that went away before the dwell was up was never announced, and
			// must not silence the next one.
			n.asked = true
		}
	}
	n.mu.Unlock()

	if stale {
		return
	}
	n.emit(eventFor(s, kind))
}

func eventFor(s *Session, kind EventKind) Event {
	v := s.View()
	detail := v.AITitle
	if detail == "" {
		detail = v.LastCommand
	}
	return Event{
		Kind:        kind,
		SessionID:   v.ID,
		SandboxID:   v.SandboxID,
		SandboxName: v.SandboxName,
		Title:       v.Title,
		Detail:      detail,
		At:          time.Now(),
	}
}
