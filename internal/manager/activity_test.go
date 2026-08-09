package manager

import (
	"strings"
	"testing"
	"time"
)

// drawn paints lines onto a screen and returns what the classifier reads back.
func drawn(lines ...string) string {
	sc := newScreen(80, 24)
	sc.Write([]byte(strings.Join(lines, "\r\n")))
	return sc.Tail(activityRows)
}

var (
	claudeWorking = []string{
		"⏺ Read(internal/manager/session.go)",
		"  ⎿  Read 438 lines",
		"",
		"✻ Cogitating… (12s · ↑ 1.4k tokens · esc to interrupt)",
	}
	claudeAsking = []string{
		"╭────────────────────────────────────────────────╮",
		"│ Edit file                                      │",
		"│ internal/manager/session.go                    │",
		"│                                                │",
		"│ Do you want to make this edit to session.go?   │",
		"│ ❯ 1. Yes                                       │",
		"│   2. Yes, allow all edits this session         │",
		"│   3. No, and tell Claude what to do (esc)      │",
		"╰────────────────────────────────────────────────╯",
	}
	// A question with an option list, taken off a real session. The choices
	// carry a paragraph each, so the cursor picking one of them is above
	// anything that gets read — the keys along the bottom are what says a
	// question is up. It offers "Esc to cancel", which a spinner all but says
	// too, and that is why a spinner has to be recognised by its interrupt
	// rather than by its escape.
	claudeChoosing = []string{
		"     Creates .nvmrc at the repo root so nvm/fnm auto-switch to 18.18.0.",
		"  2. Add .nvmrc with lts/hydrogen",
		"     Pins the Node 18 LTS line by alias instead of an exact patch.",
		"  3. Leave it as is",
		"     Keep engines-only. No new files; developers pick their own Node 18.",
		"  4. Bump engines to a newer Node",
		"     Node 18 is past end-of-life; move the project to Node 20 or 22.",
		"  5. Type something.",
		"────────────────────────────────────────────────────────────",
		"  6. Chat about this",
		"",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	}
	claudeIdle = []string{
		"╭────────────────────────────────────────────────╮",
		"│ > Try \"fix the failing test\"                   │",
		"╰────────────────────────────────────────────────╯",
		"  ? for shortcuts",
	}
)

func TestClassifyScreen(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  Activity
	}{
		{"claude working", claudeWorking, ActivityBusy},
		{"claude asking permission", claudeAsking, ActivityWaiting},
		{"claude asking a question with options", claudeChoosing, ActivityWaiting},
		{"claude at its prompt", claudeIdle, ActivityIdle},
		{
			// Capitalised by codex, and the marker is looked for either way.
			"codex working",
			[]string{"▌ Working (8s • Esc to interrupt)"},
			ActivityBusy,
		},
		{
			// codex's approval overlay, in the wording its own source uses.
			"codex asking permission",
			[]string{
				"Would you like to run the following command?",
				"",
				"  $ rm -rf build",
				"",
				"  1. Yes, proceed",
				"  2. No, and tell codex what to do differently",
				"",
				"Press Enter to confirm or Esc to cancel",
			},
			ActivityWaiting,
		},
		{
			"shell asking y/n",
			[]string{"The following packages will be upgraded: curl", "Do you want to continue? [Y/n]"},
			ActivityWaiting,
		},
		{
			"shell at its prompt",
			[]string{"$ go test ./...", "ok  	github.com/x/y	0.4s", "$"},
			ActivityIdle,
		},
		{
			// A question outranks a status line the agent left above it: it
			// stopped, so what it was doing before it stopped is over.
			"question below a stale spinner",
			append([]string{"✻ Cogitating… (esc to interrupt)"}, claudeAsking...),
			ActivityWaiting,
		},
		{"nothing on screen", []string{""}, ActivityIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyScreen(drawn(tt.lines...)); got != tt.want {
				t.Errorf("classified as %q, want %q", got, tt.want)
			}
		})
	}
}

// TestScreenShowsTheLastFrameDrawn is the reason any of this goes through a
// terminal emulator: a TUI redraws its status line by moving the cursor back
// over it, so the bytes hold every frame it has ever drawn and only the screen
// says which one is up.
func TestScreenShowsTheLastFrameDrawn(t *testing.T) {
	frames := []string{
		"✻ Cogitating… (7s · esc to interrupt)",
		"\r\x1b[K✻ Cogitating… (8s · esc to interrupt)",
		"\r\x1b[K> ",
	}

	sc := newScreen(80, 24)
	for _, f := range frames {
		sc.Write([]byte(f))
	}

	if got := classifyScreen(sc.Tail(activityRows)); got != ActivityIdle {
		t.Errorf("screen classified as %q, want %q", got, ActivityIdle)
	}
	// The same bytes, read as a stream rather than as a picture, say the
	// opposite — twice.
	if !strings.Contains(strings.Join(frames, ""), "esc to interrupt") {
		t.Fatal("the fixture no longer demonstrates the problem it is here for")
	}
}

// TestScreenReadsOnlyTheBottom keeps the transcript out of it: a prompt that
// has been answered is buried by whatever the agent did next, and prose
// scrolling past can say anything, including quoting the markers.
func TestScreenReadsOnlyTheBottom(t *testing.T) {
	lines := append([]string{}, claudeAsking...)
	for i := 0; i < activityRows+2; i++ {
		lines = append(lines, "⏺ Update(session.go)")
	}
	if got := classifyScreen(drawn(lines...)); got != ActivityIdle {
		t.Errorf("a prompt buried under later output classified as %q, want %q", got, ActivityIdle)
	}
}

// TestScreenFollowsAResize checks that the bottom of the screen is still the
// bottom after the window changes size. A resized program repaints itself, so
// what matters is the frame that lands afterwards; between the two, one sample
// can read the old picture in its new position.
func TestScreenFollowsAResize(t *testing.T) {
	sc := newScreen(80, 24)
	sc.Write([]byte(strings.Repeat("\r\n", 24) + "✻ Cogitating… (esc to interrupt)"))

	sc.Resize(100, 40)
	sc.Write([]byte("\r\x1b[K✻ Cogitating… (esc to interrupt)"))

	if got := classifyScreen(sc.Tail(activityRows)); got != ActivityBusy {
		t.Errorf("after a resize and repaint the screen classified as %q, want %q", got, ActivityBusy)
	}
}

// TestActivityBelievesTheTerminalTitle covers the one signal that is not an
// inference. codex flags an approval in the terminal title, which settles the
// question whether or not the screen is recognisable and whether or not the
// session is still drawing — an overlay that animates would otherwise read as
// working for as long as it was up.
func TestActivityBelievesTheTerminalTitle(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "codex")

	s.screen.Write([]byte("\x1b]2;[ ! ] Action Required | codex | ctx:14%\x07working away"))
	s.lastOutput = time.Now()
	if got := s.readActivity(); got != ActivityWaiting {
		t.Errorf("a session flagged in its title read %q, want %q", got, ActivityWaiting)
	}

	// Answered: the title goes back to what codex is doing, and so does the
	// session.
	s.screen.Write([]byte("\x1b]2;codex | ctx:14%\x07"))
	if got := s.readActivity(); got != ActivityBusy {
		t.Errorf("after the title cleared, the session read %q, want %q", got, ActivityBusy)
	}
}

// sessionShowing returns a quiet session with lines painted on its screen.
func sessionShowing(kind Kind, lines ...string) *Session {
	s := newSession("s1", "b1", "box", kind, nil, "agent")
	s.screen.Write([]byte(strings.Join(lines, "\r\n")))
	s.lastOutput = time.Now().Add(-2 * activityQuiet)
	return s
}

// TestAgentEndingOnAQuestionIsWaiting covers the case no widget gives away.
// codex answers with a question and drops back to its composer — the same
// empty box it shows after finishing a task, down to the grey placeholder in
// it — so the question mark is the whole of the evidence.
func TestAgentEndingOnAQuestionIsWaiting(t *testing.T) {
	asked := []string{
		"  Tip: New Use /fast to enable our fastest inference.",
		"",
		"› Ask me something",
		"",
		"• What’s something you’ve been meaning to build, learn, or change—but keep postponing?",
		"",
		"› Improve documentation in @filename",
		"",
		"  gpt-5.6-sol default · /Users/me/Projects/Lens",
	}
	finished := []string{
		"  Tip: New Use /fast to enable our fastest inference.",
		"",
		"› Tidy the imports",
		"",
		"• Removed three unused imports from main.go.",
		"",
		"› Improve documentation in @filename",
		"",
		"  gpt-5.6-sol default · /Users/me/Projects/Lens",
	}

	if got := sessionShowing(KindAgent, asked...).readActivity(); got != ActivityWaiting {
		t.Errorf("an agent that asked a question read %q, want %q", got, ActivityWaiting)
	}
	if got := sessionShowing(KindAgent, finished...).readActivity(); got != ActivityIdle {
		t.Errorf("an agent that finished quietly read %q, want %q", got, ActivityIdle)
	}
	// A shell is not in a conversation, and its prompt is full of punctuation
	// nobody meant as a question.
	if got := sessionShowing(KindShell, "$ echo 'ready?'", "ready?", "$").readActivity(); got != ActivityIdle {
		t.Errorf("a shell whose output ended in a question read %q, want %q", got, ActivityIdle)
	}
}

// TestQuestionIsOnlyTheLastThingSaid keeps an answered question from holding
// the dot amber for the rest of the session.
func TestQuestionIsOnlyTheLastThingSaid(t *testing.T) {
	lines := []string{"• Which database should I use?", "", "› proceed with sqlite", ""}
	for i := 0; i < questionLines; i++ {
		lines = append(lines, "• Wired it up.")
	}
	lines = append(lines, "", "› Improve documentation in @filename")

	if got := sessionShowing(KindAgent, lines...).readActivity(); got != ActivityIdle {
		t.Errorf("a question buried under later output read %q, want %q", got, ActivityIdle)
	}
}

func TestReadActivityFallsBackToOutputRate(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")

	// Nothing recognisable on screen, but something is drawing it. An agent
	// this manager knows nothing about still reads as working.
	s.lastOutput = time.Now()
	if got := s.readActivity(); got != ActivityBusy {
		t.Errorf("a session mid-output read %q, want %q", got, ActivityBusy)
	}

	s.lastOutput = time.Now().Add(-2 * activityQuiet)
	if got := s.readActivity(); got != ActivityIdle {
		t.Errorf("a silent session with a blank screen read %q, want %q", got, ActivityIdle)
	}

	// Silence is not idleness when the screen says the agent is part-way
	// through something: a long tool call draws nothing while it runs.
	s.screen.Write([]byte("✻ Running… (esc to interrupt)"))
	if got := s.readActivity(); got != ActivityBusy {
		t.Errorf("a silent session mid-tool-call read %q, want %q", got, ActivityBusy)
	}
}

// TestActivityFollowsALiveSession runs the whole path over a real PTY: output
// into the screen, the sampler over the screen, and the transition out the
// other side. It stands in for the agents, which cannot be run here.
func TestActivityFollowsALiveSession(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")

	// A permission prompt, and then the silence an agent waiting on one keeps.
	script := "printf '%s\\n' " + shellQuote(claudeAsking...) + "; sleep 30"
	if err := s.start("/bin/sh", []string{"-c", script}, nil, 80, 24); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.terminate()

	// Working, until it has been quiet long enough for the screen to be worth
	// believing.
	if got := s.View().Activity; got != ActivityBusy {
		t.Errorf("a session that just started read %q, want %q", got, ActivityBusy)
	}
	waitForActivity(t, s, ActivityWaiting)

	s.terminate()
	if got := s.View().Activity; got != "" {
		t.Errorf("a session that has exited read %q, want no activity at all", got)
	}
}

// TestLookingAtASessionIsNotWork covers what a tab does the moment it opens:
// it tells the PTY what size it is, and a program answers a resize by redrawing
// everything it has. That redraw is output like any other, and counting it
// would have every session read as working as soon as it was looked at.
func TestLookingAtASessionIsNotWork(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")

	// Idle at a prompt, and repainting whenever the window changes, which is
	// what every one of these TUIs does.
	script := `trap 'printf "\033[2J\033[Hprompt> "' WINCH; printf 'prompt> '; while true; do sleep 0.05; done`
	if err := s.start("/bin/sh", []string{"-c", script}, nil, 80, 24); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.terminate()
	waitForActivity(t, s, ActivityIdle)

	// A tab attaches: same size as the session was started at, then a real
	// change when the window is a different shape.
	if err := s.Resize(80, 24); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := s.Resize(100, 30); err != nil {
		t.Fatalf("resize: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.readActivity(); got != ActivityIdle {
			t.Fatalf("a session read %q after a tab attached, want %q", got, ActivityIdle)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTypingIsNotWork is the same point for the other thing a person does to a
// session they are looking at.
func TestTypingIsNotWork(t *testing.T) {
	s := newSession("s1", "b1", "box", KindShell, nil, "shell")
	if err := s.start("/bin/cat", nil, nil, 80, 24); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.terminate()
	waitForActivity(t, s, ActivityIdle)

	for _, c := range "git status" {
		if err := s.Write([]byte(string(c))); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(40 * time.Millisecond)
		if got := s.readActivity(); got != ActivityIdle {
			t.Fatalf("a session read %q while being typed into, want %q", got, ActivityIdle)
		}
	}
}

// TestWorkSurvivesBeingLookedAt is the other half of it. Ignoring output for a
// moment must not make a session that is working look as though it had
// stopped, so neither window may outlast the silence that counts as idle.
func TestWorkSurvivesBeingLookedAt(t *testing.T) {
	if echoWindow >= activityQuiet || repaintWindow >= activityQuiet {
		t.Fatalf("a nudge window (%v, %v) outlasts activityQuiet (%v): work would read as idle",
			echoWindow, repaintWindow, activityQuiet)
	}

	s := newSession("s1", "b1", "box", KindAgent, nil, "agent")
	// Drawing steadily, and saying nothing this manager recognises, so only
	// the output itself says it is working.
	script := `while true; do printf 'x'; sleep 0.1; done`
	if err := s.start("/bin/sh", []string{"-c", script}, nil, 80, 24); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer s.terminate()

	if err := s.Resize(100, 30); err != nil {
		t.Fatalf("resize: %v", err)
	}
	deadline := time.Now().Add(repaintWindow + activityQuiet + time.Second)
	for time.Now().Before(deadline) {
		if got := s.readActivity(); got != ActivityBusy {
			t.Fatalf("a working session read %q after being looked at, want %q", got, ActivityBusy)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForActivity(t *testing.T, s *Session, want Activity) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.View().Activity == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("activity never became %q (it is %q); the screen reads:\n%s",
		want, s.View().Activity, s.screen.Tail(activityRows))
}

func shellQuote(args ...string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(out, " ")
}

func TestSetActivityNotifiesWatchers(t *testing.T) {
	s := newSession("s1", "b1", "box", KindAgent, nil, "claude")
	changed, unwatch := s.Watch()
	defer unwatch()

	s.setActivity(ActivityWaiting)
	if !notified(changed) {
		t.Fatal("a change of activity did not wake the watcher")
	}

	// The sampler runs four times a second; only the transitions are worth
	// waking anyone for.
	s.setActivity(ActivityWaiting)
	if notified(changed) {
		t.Fatal("an unchanged activity woke the watcher")
	}
}
