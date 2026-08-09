package manager

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hinshun/vt10x"
)

// A session's status says whether its process is alive. Activity says what it
// appears to be doing, which is the part worth a glance when a sandbox has
// several agents in it: one is working, one has stopped on a question only the
// person can answer, and the rest are sitting at their prompts.
//
// None of it is reported by the agent. It is read off the session's own
// screen, so it is an inference and is documented as one — a session whose
// agent this manager does not recognise still reads busy while it works, and
// idle when it stops.
type Activity string

const (
	// ActivityIdle is a session sitting at a prompt with nothing to do.
	ActivityIdle Activity = "idle"
	// ActivityBusy is a session working on something.
	ActivityBusy Activity = "busy"
	// ActivityWaiting is a session stopped on a question it cannot go past
	// until it is answered. It is the one state where the person is what is
	// being waited for, which is what makes it worth telling apart from idle.
	ActivityWaiting Activity = "waiting"
)

const (
	// activitySample is how often a live session is looked at. Fast enough
	// that the dot follows the work, far slower than the ten frames a second
	// an agent's spinner draws.
	activitySample = 250 * time.Millisecond
	// activityQuiet is how long a session must draw nothing before what is on
	// its screen is believed. Every one of these TUIs animates while it works,
	// so output more recent than this means work whatever the screen says.
	activityQuiet = 1200 * time.Millisecond
	// echoWindow is how long output counts as our own doing rather than the
	// agent's after a keystroke goes in: long enough for the echo to come
	// back, short enough that a spinner underneath it is not lost for long.
	echoWindow = 300 * time.Millisecond
	// repaintWindow is the same after a resize, which a program answers by
	// redrawing everything. It is longer because a redraw is bigger than an
	// echo, and it matters more: opening a tab resizes the PTY to fit it, so
	// counting the repaint would have every session read as working the
	// moment it was looked at.
	//
	// Both windows have to stay under activityQuiet. A window that outlasts it
	// would make a session that is working look as though it had stopped, for
	// however long the difference is, just because someone looked at it.
	repaintWindow = 1000 * time.Millisecond
	// activityRows is how much of the foot of the screen is read. A prompt, a
	// spinner and a question are all drawn there; above them is the
	// transcript, which is prose, and prose matches anything.
	activityRows = 12
)

// busyMarkers are what an agent prints while a turn is running. The wording of
// a spinner changes from release to release, but the interrupt key has to be
// advertised while there is something to interrupt, so that is what is looked
// for.
//
// Only the interrupt is looked for, and not the near-identical "esc to
// cancel": a question offers that too, and there it means the opposite.
//
// These only decide a session that has gone quiet — an agent part-way through
// a long tool call, which draws nothing while it waits. Anything still
// painting the screen counts as busy without having to be recognised at all.
var busyMarkers = []string{
	"esc to interrupt",
	"ctrl+c to interrupt",
	"ctrl-c to interrupt",
}

// waitingMarkers are what a session says when it is holding for an answer: the
// keys a menu offers, and the plain y/n a shell command asks.
//
// The keys are what a list of options puts along the bottom, which is the one
// part of it that is always in view. The options themselves need not be —
// claude asks questions whose choices carry a paragraph each, and the cursor
// picking one of them can be well off the top of what is read here.
var waitingMarkers = []string{
	"enter to select",
	"to confirm",
	"to navigate",
	// codex asks in prose above the keys, and says which of these it is.
	"would you like to run",
	"would you like to make",
	"would you like to grant",
	"do you want to approve",
	"needs your approval",
	"(y/n)",
	"[y/n]",
	"(y/n/a)",
	"[y/n/a]",
	"(yes/no)",
}

// actionRequired is what codex puts at the front of the terminal title for as
// long as an approval is up, and it is the only thing any of these agents
// states outright rather than draws.
//
// So it is believed ahead of everything else, including the rule that a
// session still painting its screen is working: this is not an inference from
// a picture, it is the agent saying which state it is in.
const actionRequired = "action required"

func titleSaysWaiting(title string) bool {
	return strings.Contains(strings.ToLower(title), actionRequired)
}

// waitingRE matches the other shape a question takes: a selection cursor
// sitting on a numbered option, which is how the permission prompts are drawn.
// They sit inside a box, so the cursor is looked for anywhere on its line
// rather than at the start of one, and a digit is required — a bare cursor is
// what a shell prompt looks like, and what several of these agents draw at an
// empty input box, which is the opposite of waiting.
//
// An agent whose question looks like none of this reads as idle while it
// waits. That is the failure this degrades to, and adding a marker above is
// what fixes it.
var waitingRE = regexp.MustCompile(`[❯›▸▶]\s*\d+[.)]\s+\S`)

// classifyScreen reads what a session that has gone quiet says it is doing.
//
// A question outranks everything else on the screen: an agent that asked one
// has stopped, and whatever it was drawing before it stopped may well still be
// up there above the box.
func classifyScreen(tail string) Activity {
	tail = strings.ToLower(tail)
	if waitingRE.MatchString(tail) || containsAny(tail, waitingMarkers) {
		return ActivityWaiting
	}
	if containsAny(tail, busyMarkers) {
		return ActivityBusy
	}
	return ActivityIdle
}

// questionLines is how far back up the screen a question still counts as the
// last thing said. Between an agent's final line and the foot of the screen
// sits its input box and whatever it decorates that with — a border, a grey
// placeholder, a status line — and the question has to be found over the top
// of all of it without reaching so far back that an answered one is caught.
const questionLines = 6

// endsOnAQuestion reports whether an agent's last words were a question.
//
// It is the softest thing here, and the only one that reads meaning rather
// than recognising a widget. An agent that ends its turn by asking something
// leaves behind exactly what an agent that ends it by finishing leaves behind:
// an empty input box. Nothing on the screen separates the two, so the question
// mark has to.
//
// It follows that this both misses and over-reaches. A question put as "let me
// know which you prefer" is not caught, and a summary that ends on a rhetorical
// question is caught wrongly. Both are one dot in one sidebar, which is the
// level of wrongness this is worth.
func endsOnAQuestion(tail string) bool {
	lines := strings.Split(tail, "\n")
	for i, seen := len(lines)-1, 0; i >= 0 && seen < questionLines; i-- {
		line := strings.TrimRight(lines[i], " ")
		if line == "" {
			continue
		}
		seen++
		if strings.HasSuffix(strings.TrimRight(line, `"'’)»`), "?") {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// screen is a headless terminal fed the same bytes the browser is.
//
// What a session is doing is written on its screen, and only there. A TUI does
// not emit a transcript, it emits repaints: the status line is redrawn several
// times a second by putting the cursor back over it, so the recent output
// holds every frame ever drawn — the spinner from a second ago sits in the
// stream right beside the prompt that replaced it. Reading those bytes says
// what was true at some point. Replaying them into a screen says what is true
// now, which is the question being asked.
type screen struct {
	mu         sync.Mutex
	term       vt10x.Terminal
	cols, rows int
	// pending holds a rune split across two PTY reads, rather than letting
	// half of one be drawn as a replacement character.
	pending []byte
}

func newScreen(cols, rows int) *screen {
	return &screen{
		term: vt10x.New(vt10x.WithSize(cols, rows)),
		cols: cols,
		rows: rows,
	}
}

func (s *screen) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		p = append(s.pending, p...)
		s.pending = nil
	}
	n, err := s.term.Write(p)
	if err != nil {
		return
	}
	if n < len(p) {
		s.pending = append([]byte(nil), p[n:]...)
	}
}

func (s *screen) Resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cols <= 0 || rows <= 0 || (cols == s.cols && rows == s.rows) {
		return
	}
	s.cols, s.rows = cols, rows
	s.term.Resize(cols, rows)
}

// Reset throws the screen away, which is what a restart does to the real one.
// Whatever the previous run left up there is not what this one is doing.
func (s *screen) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.term = vt10x.New(vt10x.WithSize(s.cols, s.rows))
	s.pending = nil
}

// Title returns the terminal title the program has set, which some of them
// use to say things they never put on the screen.
func (s *screen) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.term.Lock()
	defer s.term.Unlock()
	return s.term.Title()
}

// Tail returns the last n drawn rows of the screen as plain text, with the
// blank right-hand side of each row cut away.
//
// The last drawn row, not the last row: the bottom of the screen is only where
// a program draws once it has filled the screen, and one that started a moment
// ago — or that never fills it — is painting the top. Trailing blank rows are
// no part of what it is saying either way.
func (s *screen) Tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		return ""
	}

	// vt10x guards its own state, and Cell does not take that lock itself.
	s.term.Lock()
	defer s.term.Unlock()

	last := -1
	for y := s.rows - 1; y >= 0; y-- {
		if s.rowLocked(y) != "" {
			last = y
			break
		}
	}
	if last < 0 {
		return ""
	}

	var b strings.Builder
	for y := max(0, last-n+1); y <= last; y++ {
		b.WriteString(s.rowLocked(y))
		b.WriteByte('\n')
	}
	return b.String()
}

// rowLocked reads one row of the screen. The caller holds the terminal's lock.
func (s *screen) rowLocked(y int) string {
	row := make([]rune, 0, s.cols)
	for x := 0; x < s.cols; x++ {
		c := s.term.Cell(x, y).Char
		if c == 0 {
			c = ' '
		}
		row = append(row, c)
	}
	return strings.TrimRight(string(row), " ")
}
