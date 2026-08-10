package manager

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Status is the lifecycle state of a session's process.
type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusExited   Status = "exited"
	StatusFailed   Status = "failed"
)

// Kind is what a session runs inside its sandbox.
type Kind string

const (
	// KindAgent runs the sandbox's own agent ("sbx run --name"). Every
	// attachment gets its own agent process on its own TTY, so a sandbox can
	// have several working in it at once.
	KindAgent Kind = "agent"
	// KindShell opens an interactive shell beside whatever else is running
	// ("sbx exec").
	KindShell Kind = "shell"
)

const scrollbackBytes = 256 * 1024

// Session is one terminal running inside a sandbox.
//
// Sessions are deliberately not persisted: a session is a live process with a
// PTY behind it, and there is nothing left of one after the manager exits.
// Sandboxes survive a restart; sessions are started again inside them.
type Session struct {
	ID          string `json:"id"`
	SandboxID   string `json:"sandboxId"`
	SandboxName string `json:"sandboxName"`
	Kind        Kind   `json:"kind"`
	// Title is what this session is called in the UI, derived when it starts
	// from the agent, its arguments, and the sandbox's other sessions.
	Title     string    `json:"title"`
	AgentArgs []string  `json:"agentArgs,omitempty"`
	CreatedAt time.Time `json:"createdAt"`

	mu        sync.Mutex
	status    Status
	exitCode  int
	lastErr   string
	startedAt time.Time

	// activity is what the session looks like it is doing, as distinct from
	// whether its process is alive. It is worked out from the screen and from
	// how recently anything was drawn on it; see activity.go.
	activity   Activity
	lastOutput time.Time
	// lastInput is the last time a person did something to this session:
	// started it, or typed at it. It is deliberately not moved by anything the
	// agent does on its own — a session left running overnight is not more
	// recently used than one someone spoke to an hour ago, and it is this that
	// orders sessions, sandboxes and projects in the UI.
	lastInput time.Time
	// quietUntil is when output starts counting as the agent's own again.
	// Anything drawn before it is the answer to something this manager did —
	// the echo of a keystroke, the repaint that follows a resize — and none of
	// that is the agent doing anything.
	quietUntil time.Time
	// cols and rows are the size the PTY was last set to, so that a resize
	// that changes nothing is not treated as one.
	cols, rows uint16

	// aiTitle is what the agent called this conversation itself, once it has
	// called it anything. It describes the work rather than the command, so
	// the UI shows it beside Title instead of replacing it — a session must
	// not rename itself out from under someone mid-task.
	aiTitle string
	// convID is the conversation the agent was started under, and the handle
	// its title is read back by. Empty when this session has no such title.
	convID string

	// lastCommand is the most recent command line typed at a shell session's
	// prompt, and line is the one being typed now. A shell never names its own
	// work, so this is what says what such a session is doing — shown where an
	// agent's own title would be.
	lastCommand string
	line        cmdLine

	cmd    *exec.Cmd
	ptmx   *os.File
	scroll *ringBuffer
	// screen is the same output resolved into the picture it draws, which is
	// what says whether the session is working or waiting on an answer.
	screen *screen

	subs   map[int]chan []byte
	nextID int

	// watchers are told when something in the view changes that whoever is
	// attached should see now rather than at the next poll: an agent's title,
	// a shell's command. The notification carries nothing — a watcher reads
	// the view for itself.
	watchers  map[int]chan struct{}
	nextWatch int

	// done is closed when the session's process exits.
	done chan struct{}

	// onActivity is called after every activity transition, outside s.mu, with
	// the state left behind and the one arrived at. It is what turns a
	// flickering signal into the handful of moments worth notifying someone
	// about; see notify.go. Set once when the session is registered and never
	// afterwards, so it is safe to read without the lock.
	onActivity func(s *Session, prev, next Activity)

	// onUserInput is called after someone starts or types at this session, so
	// that what owns it can record that it was just used. Set once when the
	// session is registered and never afterwards, so it is safe to read
	// without the lock.
	onUserInput func(s *Session)
}

func newSession(id, sandboxID, sandboxName string, kind Kind, agentArgs []string, title string) *Session {
	cols, rows := dims(0, 0)
	return &Session{
		ID:          id,
		SandboxID:   sandboxID,
		SandboxName: sandboxName,
		Kind:        kind,
		Title:       title,
		AgentArgs:   agentArgs,
		CreatedAt:   time.Now(),
		// Starting a session is itself something a person did, so it counts as
		// the first use of it.
		lastInput: time.Now(),
		status:    StatusStarting,
		scroll:    newRingBuffer(scrollbackBytes),
		screen:    newScreen(int(cols), int(rows)),
		subs:      make(map[int]chan []byte),
		watchers:  make(map[int]chan struct{}),
		done:      make(chan struct{}),
	}
}

// SessionView is the JSON-facing snapshot of a session.
type SessionView struct {
	ID          string    `json:"id"`
	SandboxID   string    `json:"sandboxId"`
	SandboxName string    `json:"sandboxName"`
	Kind        Kind      `json:"kind"`
	Title       string    `json:"title"`
	AITitle     string    `json:"aiTitle,omitempty"`
	LastCommand string    `json:"lastCommand,omitempty"`
	AgentArgs   []string  `json:"agentArgs,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	// LastActivityAt is the last time a person used this session — started it,
	// or typed at it. Work the agent does on its own does not move it. It is
	// what orders a sandbox's sessions most-recently-used first.
	LastActivityAt time.Time `json:"lastActivityAt"`
	Status         Status    `json:"status"`
	// Activity is what a live session looks like it is doing. It is absent for
	// one that is not running: a process that has exited is not idle, it is
	// gone, and its status says so.
	Activity  Activity   `json:"activity,omitempty"`
	ExitCode  int        `json:"exitCode"`
	Error     string     `json:"error,omitempty"`
	Clients   int        `json:"clients"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := SessionView{
		ID:             s.ID,
		SandboxID:      s.SandboxID,
		SandboxName:    s.SandboxName,
		Kind:           s.Kind,
		Title:          s.Title,
		AITitle:        s.aiTitle,
		LastCommand:    s.lastCommand,
		AgentArgs:      s.AgentArgs,
		CreatedAt:      s.CreatedAt,
		LastActivityAt: s.lastInput,
		Status:         s.status,
		Activity:       s.activity,
		ExitCode:       s.exitCode,
		Error:          s.lastErr,
		Clients:        len(s.subs),
	}
	if !s.startedAt.IsZero() {
		t := s.startedAt
		v.StartedAt = &t
	}
	return v
}

// setAITitle records the title the agent gave its own conversation.
func (s *Session) setAITitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aiTitle == title {
		return
	}
	s.aiTitle = title
	s.notifyLocked()
}

// Watch returns a channel that receives whenever the session's view changes.
// The returned function stops watching.
//
// The channel is never closed: a watcher stops when it stops watching, not
// when the session does, and a session that ends says so through Done. Sends
// are dropped when the channel is full, because a watcher one notification
// behind learns everything from the next one — the view is read afresh either
// way.
func (s *Session) Watch() (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{}, 1)
	id := s.nextWatch
	s.nextWatch++
	s.watchers[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.watchers, id)
	}
}

// notifyLocked wakes every watcher. The caller holds s.mu.
func (s *Session) notifyLocked() {
	for _, ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// setConvID records the conversation a run of this session was started under.
// Any title from a previous run described a conversation this one is not
// continuing, so it goes with it.
func (s *Session) setConvID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.convID = id
	s.aiTitle = ""
}

// ConvID returns the conversation this run of the session was started under.
func (s *Session) ConvID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convID
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// IsLive reports whether a process is currently attached.
func (s *Session) IsLive() bool {
	st := s.Status()
	return st == StatusStarting || st == StatusRunning
}

// start launches argv under a PTY and begins pumping its output to
// subscribers. It returns an error if a process is already attached.
func (s *Session) start(bin string, argv []string, env []string, cols, rows uint16) error {
	s.mu.Lock()
	if s.status == StatusRunning {
		s.mu.Unlock()
		return errors.New("session already running")
	}
	s.status = StatusStarting
	s.lastErr = ""
	s.exitCode = 0
	// Whatever the previous run was doing is not what this one is doing.
	s.lastCommand = ""
	s.line.reset()
	s.screen.Reset()
	s.screen.Resize(int(cols), int(rows))
	// Starting counts as working: the agent is booting, and a session that
	// read idle for its first second would be saying something false.
	s.activity = ActivityBusy
	s.lastOutput = time.Now()
	// Starting a session — including restarting one — is a person using it.
	s.lastInput = time.Now()
	s.quietUntil = time.Time{}
	// The size the process is started at, so the first tab to attach and say
	// what size it is does not read as a resize.
	s.cols, s.rows = cols, rows
	s.done = make(chan struct{})
	s.mu.Unlock()

	// The activity above was set rather than transitioned to, so nothing has
	// been told about it. Saying so here is what starts this run's clock: a
	// restarted session must not be credited with the work the last run did,
	// or it would report that it had finished something the moment it settled.
	if s.onActivity != nil {
		s.onActivity(s, "", ActivityBusy)
	}
	// Starting one is the one moment a session has been used before a single
	// keystroke has been sent to it.
	if s.onUserInput != nil {
		s.onUserInput(s)
	}

	cmd := exec.Command(bin, argv...)
	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		s.mu.Lock()
		s.status = StatusFailed
		s.lastErr = err.Error()
		s.activity = ""
		close(s.done)
		s.mu.Unlock()
		return err
	}

	s.mu.Lock()
	s.cmd = cmd
	s.ptmx = ptmx
	s.status = StatusRunning
	s.startedAt = time.Now()
	done := s.done
	s.mu.Unlock()

	go s.pump()
	// Handed this run's done channel rather than reading it back later: a
	// restart replaces it, and this watcher belongs to the run that started it.
	go s.watchActivity(done)
	return nil
}

// watchActivity keeps a session's activity current for as long as its process
// lives. It samples instead of reacting to output because the transition that
// matters is the one into silence — an agent that stops working stops drawing,
// and there is no event in that.
func (s *Session) watchActivity(done <-chan struct{}) {
	t := time.NewTicker(activitySample)
	defer t.Stop()
	for {
		select {
		case <-done:
			// pump clears the activity as it records the exit, so there is
			// nothing to put right here.
			return
		case <-t.C:
			s.setActivity(s.readActivity())
		}
	}
}

// readActivity works out what the session is doing at this moment.
func (s *Session) readActivity() Activity {
	// An agent that says outright that it is blocked is taken at its word,
	// before anything is inferred from output or read off the screen.
	if titleSaysWaiting(s.screen.Title()) {
		return ActivityWaiting
	}
	s.mu.Lock()
	quiet := time.Since(s.lastOutput) >= activityQuiet
	s.mu.Unlock()
	// Anything still painting the screen is working, whatever it is painting.
	// Only silence has to be interpreted, and that is the expensive half.
	if !quiet {
		return ActivityBusy
	}
	tail := s.screen.Tail(activityRows)
	if a := classifyScreen(tail); a != ActivityIdle {
		return a
	}
	// Nothing on the screen is a prompt, so the last thing said is all there
	// is to go on. An agent that ended its turn on a question is waiting for
	// the answer; a shell is asked nothing and answers nothing, and the
	// confirmations it does put up are recognised above.
	if s.Kind == KindAgent && endsOnAQuestion(tail) {
		return ActivityWaiting
	}
	return ActivityIdle
}

func (s *Session) setActivity(a Activity) {
	s.mu.Lock()
	prev := s.activity
	if prev == a {
		s.mu.Unlock()
		return
	}
	s.activity = a
	s.notifyLocked()
	s.mu.Unlock()

	// Outside the lock: what this ends up calling reads the session's view,
	// which takes the same lock.
	if s.onActivity != nil {
		s.onActivity(s, prev, a)
	}
}

// pump copies PTY output into the scrollback buffer and to every attached
// client until the process exits.
func (s *Session) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.scroll.Write(chunk)
			s.screen.Write(chunk)
			s.noteOutput()
			s.broadcast(chunk)
		}
		if err != nil {
			break
		}
	}

	err := s.cmd.Wait()

	s.mu.Lock()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			s.exitCode = ee.ExitCode()
			s.status = StatusExited
		} else {
			s.status = StatusFailed
			s.lastErr = err.Error()
		}
	} else {
		s.exitCode = 0
		s.status = StatusExited
	}
	// A process that has exited is not idle, it is gone. Cleared here rather
	// than in the watcher so there is no window where a dead session still
	// claims to be working.
	prev := s.activity
	s.activity = ""
	_ = s.ptmx.Close()
	s.ptmx = nil
	s.cmd = nil
	close(s.done)
	s.mu.Unlock()

	// A session that ended has not finished a task, whatever it was doing a
	// moment ago; this withdraws anything that was about to be said about it.
	if s.onActivity != nil {
		s.onActivity(s, prev, "")
	}

	s.broadcast([]byte("\r\n\x1b[38;5;244m[session ended]\x1b[0m\r\n"))
	s.closeSubs()
}

// noteOutput records that the session drew something, which is most of what
// says it is working — unless this manager is what made it draw.
func (s *Session) noteOutput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Before(s.quietUntil) {
		return
	}
	s.lastOutput = now
}

// nudge records that the session was just prodded, and for how long what comes
// back should be read as the answer to that rather than as work.
//
// An agent that really is working goes on saying so once the window passes,
// both by drawing again and — for the agents whose spinners are recognised —
// on the screen itself, which is read whenever output alone is not enough.
func (s *Session) nudge(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until := time.Now().Add(d); until.After(s.quietUntil) {
		s.quietUntil = until
	}
}

func (s *Session) broadcast(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		select {
		case ch <- p:
		default:
			// A stalled client must not block the agent: drop it.
			close(ch)
			delete(s.subs, id)
		}
	}
}

func (s *Session) closeSubs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		close(ch)
		delete(s.subs, id)
	}
}

// Subscribe returns the current scrollback plus a channel of subsequent
// output. The returned function detaches the subscriber.
func (s *Session) Subscribe() (scrollback []byte, out <-chan []byte, cancel func()) {
	scrollback = s.scroll.Bytes()

	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan []byte, 256)
	id := s.nextID
	s.nextID++
	s.subs[id] = ch

	return scrollback, ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			close(c)
			delete(s.subs, id)
		}
	}
}

// Write forwards keystrokes to the session's process.
func (s *Session) Write(p []byte) error {
	s.mu.Lock()
	ptmx := s.ptmx
	s.mu.Unlock()
	if ptmx == nil {
		return errors.New("session is not running")
	}
	if _, err := ptmx.Write(p); err != nil {
		return err
	}
	// What comes back next is the echo of this, not the agent at work.
	s.nudge(echoWindow)
	s.noteInput(p)
	s.noteUsed()
	return nil
}

// noteUsed records that a person just did something to this session, and tells
// whatever owns it so the sandbox and project it belongs to move with it.
func (s *Session) noteUsed() {
	s.mu.Lock()
	s.lastInput = time.Now()
	s.mu.Unlock()
	if s.onUserInput != nil {
		s.onUserInput(s)
	}
}

// noteInput follows what is being typed at a shell's prompt. Only a shell has
// a command line to follow: an agent's keystrokes are answers to whatever it
// is asking, and it says what it is doing itself.
func (s *Session) noteInput(p []byte) {
	if s.Kind != KindShell {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := s.line.feed(p)
	if cmd == "" {
		return
	}
	if cmd = truncate(cmd, maxCommandTitle); cmd == s.lastCommand {
		return
	}
	s.lastCommand = cmd
	s.notifyLocked()
}

// Resize updates the PTY window size.
func (s *Session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	s.mu.Lock()
	ptmx := s.ptmx
	unchanged := cols == s.cols && rows == s.rows
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	if ptmx == nil {
		return nil
	}
	// Every tab says what size it is the moment it attaches, and most of the
	// time that is the size the PTY already has. Passing it on anyway would
	// send the program a signal to redraw for nothing — and looking at a
	// session is not the session doing anything.
	if unchanged {
		return nil
	}
	// The headless screen has to be the size the program is drawing for, or it
	// wraps its lines somewhere else and the bottom rows stop being the bottom
	// rows. Several tabs of different sizes fight over this exactly as they
	// already fight over the PTY.
	s.screen.Resize(int(cols), int(rows))
	// A program answers a resize by redrawing everything it has on screen.
	s.nudge(repaintWindow)
	return pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// signal sends sig to the session's process, if one is attached.
func (s *Session) signal(sig os.Signal) error {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("session is not running")
	}
	return cmd.Process.Signal(sig)
}

// Done returns a channel closed when the session's process exits.
func (s *Session) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// terminate stops the session's process, escalating to SIGKILL if it does not
// go away on its own.
func (s *Session) terminate() {
	if !s.IsLive() {
		return
	}
	_ = s.signal(syscall.SIGTERM)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		_ = s.signal(syscall.SIGKILL)
		<-s.Done()
	}
}
