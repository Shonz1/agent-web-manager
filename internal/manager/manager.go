// Package manager owns the sandboxes this tool manages and the terminal
// sessions running inside them.
//
// The sandbox is the durable entity: it is created once, persisted, and
// outlives this process. Sessions are the ephemeral ones — a PTY attached to
// an agent or a shell inside a sandbox — and several can run in one sandbox
// at a time.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

var (
	ErrSandboxNotFound = errors.New("sandbox not found")
	ErrSessionNotFound = errors.New("session not found")
	ErrExists          = errors.New("a sandbox with that name already exists")
	// ErrBaseSandbox rejects working in or destroying a project's base
	// sandbox. It is the sandbox every session sandbox is cloned from, so
	// running in it would change what the next session inherits, and deleting
	// it would leave the project with nothing to clone — and there is nothing
	// it does that a session sandbox does not do better. It goes when its
	// project goes. Stopping it is allowed: it holds no work, and whatever
	// needs it next starts it again.
	ErrBaseSandbox = errors.New("this is the project's base sandbox: it is only ever cloned from, so nothing can be run in it and it cannot be deleted on its own")
)

// createTimeout covers an "sbx create" that has to pull an agent image, which
// is far longer than any other sbx call the manager makes.
const createTimeout = 10 * time.Minute

// removeTimeout covers an "sbx rm" the manager does on its own account rather
// than on behalf of a request: tearing a container down asks nothing of the
// network, so it is the same minute the API gives a delete someone asked for.
const removeTimeout = time.Minute

// eventBuffer is how many notification events a subscriber can fall behind by.
// They arrive minutes apart at worst and are held back by a dwell before they
// are sent at all, so anything approaching this is a subscriber that has
// stopped reading rather than a burst.
const eventBuffer = 32

// Manager owns the set of sandboxes and the sessions running inside them.
type Manager struct {
	client   *sbx.Client
	stateDir string

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox // by sandbox ID
	byName    map[string]string   // sandbox name -> sandbox ID
	sessions  map[string]*Session // by session ID, across all sandboxes
	projects  map[string]*Project // by project ID

	// Events have a lock of their own. They are emitted from a timer goroutine
	// that may be holding nothing at all, and sharing mu would put the
	// sandbox map behind whatever a subscriber is doing.
	evMu      sync.Mutex
	eventSubs map[int]chan Event
	nextEvent int

	// Making a project's base sandbox can take an image pull's worth of time,
	// and both the project create and the first session want it: they are
	// serialised per project so the loser waits for the winner's sandbox
	// instead of starting a second one. baseMu guards the map; the mutexes in
	// it are held across the create itself, which is why one shared lock
	// would not do — a slow pull for one project would stall every other.
	baseMu    sync.Mutex
	baseLocks map[string]*sync.Mutex

	// A session's activity can flip several times a second, and touching a
	// sandbox's LastActivityAt is cheap, but writing it to disk is not — so
	// only the in-memory value is updated on every touch, and this throttles
	// how often that gets persisted. actMu guards the three fields below it.
	actMu       sync.Mutex
	actDirty    bool
	actLastSave time.Time
}

// activityFlushInterval bounds how often an activity touch is allowed to
// write sandboxes.json. The ordering it produces only has to survive a
// restart, not capture every flicker along the way.
const activityFlushInterval = 3 * time.Second

// New loads any previously persisted sandboxes from stateDir.
func New(client *sbx.Client, stateDir string) (*Manager, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	m := &Manager{
		client:    client,
		stateDir:  stateDir,
		sandboxes: make(map[string]*Sandbox),
		byName:    make(map[string]string),
		sessions:  make(map[string]*Session),
		projects:  make(map[string]*Project),
		eventSubs: make(map[int]chan Event),
		baseLocks: make(map[string]*sync.Mutex),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := m.loadProjects(); err != nil {
		return nil, err
	}
	return m, nil
}

// --- sandboxes ---

// CreateSandboxRequest describes a sandbox the user wants to create.
type CreateSandboxRequest struct {
	Name            string   `json:"name"`
	Agent           string   `json:"agent"`
	Workspace       string   `json:"workspace"`
	ExtraWorkspaces []string `json:"extraWorkspaces"`
	Publish         []string `json:"publish"`

	// ProjectID, IsBase, Clone, IsWorktree, and RepoRoot are set by the
	// project session flow rather than by a caller creating a sandbox
	// directly; see EnsureBaseSandbox, CreateSessionSandbox, and the worktree
	// session handler.
	ProjectID  string `json:"projectId,omitempty"`
	IsBase     bool   `json:"isBase,omitempty"`
	Clone      bool   `json:"clone,omitempty"`
	IsWorktree bool   `json:"isWorktree,omitempty"`
	RepoRoot   string `json:"repoRoot,omitempty"`

	// PluginsFrom names the sandbox whose Claude Code configuration — its
	// plugins, and the model it is set to — the new one should be given a
	// copy of. Empty takes them from the machine this manager runs on, which
	// is where a user who installs a plugin or picks a model normally does
	// it. See plugins.go and model.go for why a new sandbox has neither of
	// its own.
	PluginsFrom string `json:"pluginsFrom"`
	// NoPlugins leaves the new sandbox with whatever its image came with,
	// which is no plugins and the default model.
	NoPlugins bool `json:"noPlugins"`
}

// CreateSandbox creates a sandbox and registers it. No session is started —
// the sandbox sits there until the user starts one in it.
func (m *Manager) CreateSandbox(req CreateSandboxRequest) (*Sandbox, error) {
	if !sbx.ValidAgent(req.Agent) {
		return nil, fmt.Errorf("unknown agent %q", req.Agent)
	}
	if req.Name == "" {
		// The UI no longer names sandboxes at all, so a default that collides
		// has nobody left to override it by hand: number it the way session
		// titles are numbered rather than failing the create.
		name, err := m.uniqueSandboxName(defaultName(req.Agent, req.Workspace))
		if err != nil {
			return nil, err
		}
		req.Name = name
	}
	if !sbx.ValidName(req.Name) {
		return nil, fmt.Errorf("invalid sandbox name %q: use letters, numbers, and . + -", req.Name)
	}
	ws, err := resolveWorkspace(req.Workspace)
	if err != nil {
		return nil, err
	}

	extras := make([]string, 0, len(req.ExtraWorkspaces))
	for _, e := range req.ExtraWorkspaces {
		resolved, err := resolveExtraWorkspace(e)
		if err != nil {
			return nil, err
		}
		extras = append(extras, resolved)
	}

	m.mu.RLock()
	_, taken := m.byName[req.Name]
	m.mu.RUnlock()
	if taken {
		return nil, ErrExists
	}

	// Detached from the request: an image pull must not be cancelled just
	// because the browser gave up waiting for the response.
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	if err := m.client.Create(ctx, sbx.CreateOptions{
		Name:            req.Name,
		Agent:           req.Agent,
		Workspace:       ws,
		ExtraWorkspaces: extras,
		Publish:         req.Publish,
		Clone:           req.Clone,
	}); err != nil {
		return nil, err
	}

	now := time.Now()
	sb := &Sandbox{
		ID:              newID(),
		Name:            req.Name,
		Agent:           req.Agent,
		Workspace:       ws,
		ExtraWorkspaces: extras,
		Publish:         req.Publish,
		CreatedAt:       now,
		LastActivityAt:  now,
		ProjectID:       req.ProjectID,
		IsBase:          req.IsBase,
		Clone:           req.Clone,
		IsWorktree:      req.IsWorktree,
		RepoRoot:        req.RepoRoot,
	}

	m.mu.Lock()
	if _, taken := m.byName[sb.Name]; taken {
		m.mu.Unlock()
		return nil, ErrExists
	}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID
	m.mu.Unlock()

	if err := m.saveSandboxes(); err != nil {
		return nil, err
	}

	// After the sandbox is registered, so that a copy which goes wrong leaves
	// a sandbox the user can still see and use, rather than one this manager
	// has forgotten about.
	if from, ok := m.configSource(req); ok {
		m.mirrorModel(sb.Name, from)
		m.mirrorPlugins(sb.Name, from)
	}
	return sb, nil
}

// GetSandbox returns a sandbox by ID.
func (m *Manager) GetSandbox(id string) (*Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return nil, ErrSandboxNotFound
	}
	return sb, nil
}

// ListSandboxes returns every sandbox, most recently active first, with its
// live status and its sessions.
func (m *Manager) ListSandboxes(ctx context.Context) []SandboxView {
	status := m.sandboxStatuses(ctx)

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]SandboxView, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, m.viewLocked(sb, status))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivityAt.After(out[j].LastActivityAt) })
	return out
}

// SandboxView returns one sandbox with its live status and sessions.
func (m *Manager) SandboxView(ctx context.Context, id string) (SandboxView, error) {
	status := m.sandboxStatuses(ctx)

	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return SandboxView{}, ErrSandboxNotFound
	}
	return m.viewLocked(sb, status), nil
}

// viewLocked assembles a sandbox view. The caller holds m.mu.
func (m *Manager) viewLocked(sb *Sandbox, status map[string]string) SandboxView {
	v := SandboxView{Sandbox: *sb, Status: StatusMissing, Sessions: []SessionView{}}
	if st, ok := status[sb.Name]; ok {
		v.Status = st
	}
	for _, s := range m.sessions {
		if s.SandboxID == sb.ID {
			v.Sessions = append(v.Sessions, s.View())
		}
	}
	sort.Slice(v.Sessions, func(i, j int) bool {
		return v.Sessions[i].LastActivityAt.After(v.Sessions[j].LastActivityAt)
	})
	return v
}

// sandboxStatuses maps sandbox name to the status sbx reports. A failure to
// reach sbx yields an empty map, which reads as "missing" everywhere — the
// same thing the UI shows when the container really is gone.
func (m *Manager) sandboxStatuses(ctx context.Context) map[string]string {
	status := map[string]string{}
	if boxes, err := m.client.List(ctx); err == nil {
		for _, b := range boxes {
			status[b.Name] = b.Status
		}
	}
	return status
}

// ManagedNames reports the sandbox names this manager already knows about.
func (m *Manager) ManagedNames() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make(map[string]bool, len(m.byName))
	for name := range m.byName {
		names[name] = true
	}
	return names
}

// StopSandbox ends every session in the sandbox and stops the container,
// keeping its state.
//
// A base sandbox is stopped like any other. Nothing runs in one, so stopping
// it costs nothing and ends no work — and refusing would leave every project
// holding a container up for as long as the project exists, with no way to
// put it down. What needs it next starts it again: "sbx exec" does that of
// its own accord, and a create clones from the sandbox rather than from
// anything running in it.
func (m *Manager) StopSandbox(ctx context.Context, id string) error {
	sb, err := m.GetSandbox(id)
	if err != nil {
		return err
	}
	for _, s := range m.sandboxSessions(sb.ID) {
		s.terminate()
		m.dropSession(s.ID)
	}
	return m.client.Stop(ctx, sb.Name)
}

// DeleteSandbox removes the sandbox record and destroys the container. This is
// irreversible. A project's base sandbox is refused: it goes when its project
// goes, through deleteSandbox below.
func (m *Manager) DeleteSandbox(ctx context.Context, id string) error {
	sb, err := m.GetSandbox(id)
	if err != nil {
		return err
	}
	if sb.IsBase {
		return ErrBaseSandbox
	}
	return m.deleteSandbox(ctx, sb)
}

// deleteSandbox is DeleteSandbox without the base-sandbox guard, for the one
// caller entitled to ignore it: DeleteProject, which is removing the project
// the base sandbox exists for.
func (m *Manager) deleteSandbox(ctx context.Context, sb *Sandbox) error {
	for _, s := range m.sandboxSessions(sb.ID) {
		s.terminate()
		m.dropSession(s.ID)
	}

	// Remove the container first: if that fails the sandbox stays visible so
	// the user can retry instead of losing track of a live container.
	if _, exists, err := m.client.Get(ctx, sb.Name); err == nil && exists {
		if err := m.client.Remove(ctx, sb.Name); err != nil {
			return err
		}
	}

	m.mu.Lock()
	delete(m.sandboxes, sb.ID)
	delete(m.byName, sb.Name)
	m.mu.Unlock()

	return m.saveSandboxes()
}

// --- events ---

// Events returns a channel of the moments worth notifying someone about — an
// agent that has stopped on a question, or one that has finished a stretch of
// work. The returned function stops the subscription.
//
// Unlike a session's Watch, these carry their payload: a subscriber is a
// notifier, not a view, and by the time it reads one the session may well have
// moved on to something else.
func (m *Manager) Events() (<-chan Event, func()) {
	m.evMu.Lock()
	defer m.evMu.Unlock()

	ch := make(chan Event, eventBuffer)
	id := m.nextEvent
	m.nextEvent++
	m.eventSubs[id] = ch

	return ch, func() {
		m.evMu.Lock()
		defer m.evMu.Unlock()
		delete(m.eventSubs, id)
	}
}

// emit hands an event to every subscriber.
//
// A full subscriber is dropped rather than waited for — an agent must not be
// held up by a browser that stopped reading — but it is logged, because unlike
// a missed view change there is nothing to catch up from: the whole point of
// one of these is that it happened.
func (m *Manager) emit(ev Event) {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	for _, ch := range m.eventSubs {
		select {
		case ch <- ev:
		default:
			log.Printf("notify: dropped %s for session %s: subscriber is not keeping up", ev.Kind, ev.SessionID)
		}
	}
}

// --- sessions ---

// StartSessionRequest describes a terminal to open inside a sandbox.
type StartSessionRequest struct {
	Kind      Kind     `json:"kind"`
	AgentArgs []string `json:"agentArgs"`
	Cols      uint16   `json:"cols"`
	Rows      uint16   `json:"rows"`
}

// StartSession opens a new terminal inside a sandbox, starting the container
// first if it is missing.
func (m *Manager) StartSession(ctx context.Context, sandboxID string, req StartSessionRequest) (*Session, error) {
	sb, err := m.GetSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	// The base sandbox is a source to clone, not a place to work: a terminal
	// in it would change what the next session inherits, and every session
	// has a sandbox of its own to be that terminal instead.
	if sb.IsBase {
		return nil, ErrBaseSandbox
	}
	if req.Kind == "" {
		req.Kind = KindAgent
	}

	if req.Kind != KindAgent && req.Kind != KindShell {
		return nil, fmt.Errorf("unknown session kind %q", req.Kind)
	}
	argv, convID := sessionArgv(sb, req.Kind, req.AgentArgs)

	if err := m.ensureSandbox(ctx, sb); err != nil {
		return nil, err
	}

	// Titling and registration happen together so two simultaneous starts
	// cannot settle on the same name.
	m.mu.Lock()
	title := m.uniqueTitleLocked(sb, req.Kind, req.AgentArgs)
	s := newSession(newID(), sb.ID, sb.Name, req.Kind, req.AgentArgs, title)
	// Each session gets a notifier of its own: the dwell it applies is a
	// judgement about that session's own run of work.
	notifier := newSessionNotifier(m.emit, defaultDwell)
	s.onActivity = func(sess *Session, prev, next Activity) {
		notifier.activityChanged(sess, prev, next)
	}
	// A sandbox's place in the list is when someone last used it, not just when
	// it was made — this is what keeps it current across a restart. Only what a
	// person does counts: an agent left working on its own moves nothing, or
	// every list would be ordered by which agent talks the most.
	s.onUserInput = func(*Session) { m.touchSandboxActivity(sb.ID) }
	m.sessions[s.ID] = s
	m.mu.Unlock()

	s.setConvID(convID)
	cols, rows := dims(req.Cols, req.Rows)
	if err := s.start(m.client.Bin, argv, agentEnv(), cols, rows); err != nil {
		m.dropSession(s.ID)
		return nil, err
	}
	m.watchTitle(s, sb)
	return s, nil
}

// sessionArgv builds what a session runs, and reports the conversation it was
// pinned to — empty unless the agent is one that can be told which
// conversation to be.
func sessionArgv(sb *Sandbox, kind Kind, agentArgs []string) (argv []string, convID string) {
	if kind == KindShell {
		return sbx.ShellArgs(sb.Name), ""
	}
	// Each "sbx run" attachment gets its own agent process on its own TTY, so
	// several can run in one sandbox without interfering.
	reader, ok := titleReaders[sb.Agent]
	if !ok || reader.pin == nil {
		return sbx.AttachArgs(sb.Name, agentArgs), ""
	}
	pin := reader.pin(agentArgs)
	runArgs := agentArgs
	if len(pin.args) > 0 {
		runArgs = append(append([]string{}, pin.args...), agentArgs...)
	}
	return sbx.AttachArgs(sb.Name, runArgs), pin.id
}

// uniqueTitleLocked names a session after what it is, and disambiguates it
// against the sandbox's other sessions. The caller holds m.mu.
//
// Numbers are only appended when they are needed, and a session that ends
// gives its number back, so a sandbox running one of each reads "claude" and
// "shell" rather than "claude 1" and "shell 1".
func (m *Manager) uniqueTitleLocked(sb *Sandbox, kind Kind, agentArgs []string) string {
	base := baseTitle(sb.Agent, kind, agentArgs)

	taken := make(map[string]bool)
	for _, s := range m.sessions {
		if s.SandboxID == sb.ID {
			taken[s.Title] = true
		}
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s %d", base, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// ensureSandbox recreates a sandbox that sbx no longer has. It is only ever
// worth doing for a sandbox whose workspace is somewhere else: rebuilt from
// this record, it is mounted on the same folder and holds the same files, so
// the session that starts in it is the session that would have started in the
// one that is gone.
//
// An adopted sandbox is only ever a mirror of one someone else set up, so
// rebuilding it from the metadata read back would quietly produce a different
// sandbox. A clone sandbox is worse: its workspace was made inside the
// container, so a rebuild is a fresh clone of the project folder, holding none
// of the work done in it — a sandbox that looks like the same one and has
// forgotten everything. Neither is done on the way to starting a terminal.
func (m *Manager) ensureSandbox(ctx context.Context, sb *Sandbox) error {
	_, exists, err := m.client.Get(ctx, sb.Name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if sb.Adopted {
		return fmt.Errorf("sandbox %q no longer exists and was not created by this manager, so it cannot be recreated here", sb.Name)
	}
	if sb.Clone {
		return fmt.Errorf("sandbox %q no longer exists, and its workspace was a clone that only ever existed inside it: rebuilding it would give you an empty one under the same name, so anything not pushed from it is gone — delete it and start a new session", sb.Name)
	}

	createCtx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	return m.client.Create(createCtx, sbx.CreateOptions{
		Name:            sb.Name,
		Agent:           sb.Agent,
		Workspace:       sb.Workspace,
		ExtraWorkspaces: sb.ExtraWorkspaces,
		Publish:         sb.Publish,
	})
}

// GetSession returns a session by ID.
func (m *Manager) GetSession(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// RestartSession re-runs a session whose process has exited, in the same
// sandbox and with the same arguments.
func (m *Manager) RestartSession(ctx context.Context, id string, cols, rows uint16) (*Session, error) {
	s, err := m.GetSession(id)
	if err != nil {
		return nil, err
	}
	if s.IsLive() {
		return nil, errors.New("session is already running")
	}
	sb, err := m.GetSandbox(s.SandboxID)
	if err != nil {
		return nil, err
	}
	if err := m.ensureSandbox(ctx, sb); err != nil {
		return nil, err
	}

	// A restart is a new conversation, not a resumption of the one that
	// exited, so it gets a conversation ID of its own.
	argv, convID := sessionArgv(sb, s.Kind, s.AgentArgs)

	s.setConvID(convID)
	c, r := dims(cols, rows)
	if err := s.start(m.client.Bin, argv, agentEnv(), c, r); err != nil {
		return nil, err
	}
	m.watchTitle(s, sb)
	return s, nil
}

// InterruptSession writes ETX to the PTY, which is exactly what pressing
// Ctrl-C in a terminal does. Signalling the sbx client instead would tear down
// the attachment rather than interrupt what is running inside the sandbox.
func (m *Manager) InterruptSession(id string) error {
	s, err := m.GetSession(id)
	if err != nil {
		return err
	}
	return s.Write([]byte{0x03})
}

// CloseSession ends a session and forgets it. The sandbox is untouched.
func (m *Manager) CloseSession(id string) error {
	s, err := m.GetSession(id)
	if err != nil {
		return err
	}
	s.terminate()
	m.dropSession(id)
	return nil
}

func (m *Manager) sandboxSessions(sandboxID string) []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.SandboxID == sandboxID {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) dropSession(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// touchSandboxActivity records that a person just used a session in
// sandboxID. The in-memory record is updated immediately, on the sandbox and
// on the project that owns it, if any; the write to disk is throttled by
// scheduleActivitySave.
func (m *Manager) touchSandboxActivity(sandboxID string) {
	m.mu.Lock()
	sb, ok := m.sandboxes[sandboxID]
	if ok {
		now := time.Now()
		sb.LastActivityAt = now
		if sb.ProjectID != "" {
			if p, ok := m.projects[sb.ProjectID]; ok {
				p.LastActivityAt = now
			}
		}
	}
	m.mu.Unlock()
	if ok {
		m.scheduleActivitySave()
	}
}

// scheduleActivitySave persists sandbox and project state at most once per
// activityFlushInterval. A touch that arrives too soon after the last write
// just marks the state dirty, and it is the next touch — whenever it comes —
// that flushes it. There is deliberately no timer to do that on its own: a
// background goroutine still writing after the process has been told to stop
// is worse than losing a few seconds of ordering, which is all that is ever
// at stake here. Shutdown flushes whatever a timer would otherwise have
// caught.
func (m *Manager) scheduleActivitySave() {
	m.actMu.Lock()
	defer m.actMu.Unlock()
	if time.Since(m.actLastSave) < activityFlushInterval {
		m.actDirty = true
		return
	}
	m.actDirty = false
	m.actLastSave = time.Now()
	_ = m.saveSandboxes()
	_ = m.saveProjects()
}

// flushActivity writes out an activity update that scheduleActivitySave held
// back. Used at shutdown, where there may be no further touch to trigger it.
func (m *Manager) flushActivity() {
	m.actMu.Lock()
	m.actDirty = false
	m.actLastSave = time.Now()
	m.actMu.Unlock()
	_ = m.saveSandboxes()
	_ = m.saveProjects()
}

// Shutdown terminates every session. Sandboxes are left running, so they and
// their state are still there on the next start.
func (m *Manager) Shutdown() {
	// Whatever the last few seconds of activity left dirty must not be lost:
	// there will be no later touch to flush it.
	m.actMu.Lock()
	dirty := m.actDirty
	m.actMu.Unlock()
	if dirty {
		m.flushActivity()
	}

	m.mu.RLock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()

	for _, s := range all {
		if s.IsLive() {
			_ = s.signal(syscall.SIGTERM)
		}
	}
	deadline := time.After(5 * time.Second)
	for _, s := range all {
		if !s.IsLive() {
			continue
		}
		select {
		case <-s.Done():
		case <-deadline:
			_ = s.signal(syscall.SIGKILL)
		}
	}
}

// --- persistence ---

func (m *Manager) statePath() string {
	return filepath.Join(m.stateDir, "sandboxes.json")
}

// legacyStatePath is where this tool wrote state back when a session and its
// sandbox were one and the same. It is read once, so an existing install
// keeps its sandboxes.
func (m *Manager) legacyStatePath() string {
	return filepath.Join(m.stateDir, "sessions.json")
}

func (m *Manager) saveSandboxes() error {
	m.mu.RLock()
	all := make([]Sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		all = append(all, *sb)
	}
	m.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })

	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("persist sandboxes: %w", err)
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		return fmt.Errorf("persist sandboxes: %w", err)
	}
	return nil
}

func (m *Manager) load() error {
	path, data, err := readFirst(m.statePath(), m.legacyStatePath())
	if err != nil {
		return err
	}
	if data == nil {
		return nil
	}
	var all []Sandbox
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("parse sandbox state (%s): %w", path, err)
	}
	for i := range all {
		sb := all[i]
		// A record written before LastActivityAt existed, or migrated from the
		// legacy sessions.json, has no better answer than when it was created.
		if sb.LastActivityAt.IsZero() {
			sb.LastActivityAt = sb.CreatedAt
		}
		m.sandboxes[sb.ID] = &sb
		m.byName[sb.Name] = sb.ID
	}
	// The legacy file stays where it is; writing the new one is what makes
	// the migration stick.
	if path == m.legacyStatePath() {
		return m.saveSandboxes()
	}
	return nil
}

// projectsPath is separate from sandboxes.json: projects are a newer concept
// than sandboxes, and an install predating them simply has no such file yet.
func (m *Manager) projectsPath() string {
	return filepath.Join(m.stateDir, "projects.json")
}

func (m *Manager) saveProjects() error {
	m.mu.RLock()
	all := make([]Project, 0, len(m.projects))
	for _, p := range m.projects {
		all = append(all, *p)
	}
	m.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })

	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.projectsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("persist projects: %w", err)
	}
	if err := os.Rename(tmp, m.projectsPath()); err != nil {
		return fmt.Errorf("persist projects: %w", err)
	}
	return nil
}

func (m *Manager) loadProjects() error {
	data, err := os.ReadFile(m.projectsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file %s: %w", m.projectsPath(), err)
	}
	var all []Project
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("parse project state (%s): %w", m.projectsPath(), err)
	}
	for i := range all {
		p := all[i]
		if p.LastActivityAt.IsZero() {
			p.LastActivityAt = p.CreatedAt
		}
		// A project written before the agent belonged to the project rather
		// than to each of its sandboxes takes the agent its sandboxes are
		// already built for, so that the base sandbox made for it on the next
		// start is the same kind of thing they are.
		if p.Agent == "" {
			p.Agent = m.projectAgentLocked(p.ID)
		}
		m.projects[p.ID] = &p
	}
	return nil
}

// projectAgentLocked guesses a project's agent from the sandboxes it already
// owns, for a record that predates the field. Sandboxes are loaded before
// projects are, so this reads the finished map; nothing else is running yet,
// which is why it takes no lock.
func (m *Manager) projectAgentLocked(projectID string) string {
	var best *Sandbox
	for _, sb := range m.sandboxes {
		if sb.ProjectID != projectID || !sbx.ValidAgent(sb.Agent) {
			continue
		}
		// The oldest sandbox mounted on the project's own folder is the
		// closest thing the old model had to a project-wide agent; a worktree
		// one will do if that is all there is.
		if best == nil || better(sb, best) {
			best = sb
		}
	}
	if best != nil {
		return best.Agent
	}
	// A project with no sandbox to learn from, which is what an install that
	// never started a session in it looks like.
	return agentClaude
}

// better reports whether a is a more telling example of a project's agent
// than b: a sandbox on the project folder over one on a worktree, and the
// older of two alike.
func better(a, b *Sandbox) bool {
	if a.IsWorktree != b.IsWorktree {
		return !a.IsWorktree
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// readFirst returns the contents of the first of paths that exists, or a nil
// slice if none do.
func readFirst(paths ...string) (string, []byte, error) {
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return p, nil, fmt.Errorf("read state file %s: %w", p, err)
		}
		return p, data, nil
	}
	return "", nil, nil
}
