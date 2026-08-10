package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/sbx"
)

// Before sandboxes and sessions were separate things, state was a list of
// sessions in sessions.json. Each of those was really a sandbox, so an
// existing install has to come back with its sandboxes intact.
func TestLoadMigratesLegacySessionState(t *testing.T) {
	dir := t.TempDir()
	legacy := `[
	  {
	    "id": "abc123",
	    "name": "claude-my-app",
	    "agent": "claude",
	    "workspace": "/w/app",
	    "extraWorkspaces": ["/w/docs:ro"],
	    "agentArgs": ["--continue"],
	    "publish": ["3000:3000"],
	    "createdAt": "2026-01-02T03:04:05Z",
	    "adopted": true
	  }
	]`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sb, err := m.GetSandbox("abc123")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if sb.Name != "claude-my-app" || sb.Agent != "claude" || sb.Workspace != "/w/app" {
		t.Errorf("got %+v", sb)
	}
	if !slices.Equal(sb.ExtraWorkspaces, []string{"/w/docs:ro"}) {
		t.Errorf("extra workspaces = %q", sb.ExtraWorkspaces)
	}
	if !slices.Equal(sb.Publish, []string{"3000:3000"}) {
		t.Errorf("publish = %q", sb.Publish)
	}
	if !sb.Adopted {
		t.Error("adopted flag should survive the migration")
	}
	if !m.ManagedNames()["claude-my-app"] {
		t.Error("migrated sandbox should be indexed by name")
	}

	// The migration only sticks once the new file is on disk.
	if _, err := os.Stat(filepath.Join(dir, "sandboxes.json")); err != nil {
		t.Fatalf("sandboxes.json not written: %v", err)
	}

	again, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := again.GetSandbox("abc123"); err != nil {
		t.Fatalf("sandbox missing after reload: %v", err)
	}
}

func TestSandboxRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}

	// Registering by hand: CreateSandbox would shell out to sbx.
	sb := &Sandbox{ID: "id1", Name: "box", Agent: "shell", Workspace: "/w"}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID
	if err := m.saveSandboxes(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetSandbox("id1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if got.Name != "box" || got.Agent != "shell" || got.Workspace != "/w" {
		t.Errorf("got %+v", got)
	}
}

// A sandbox sbx has lost is rebuilt on the way to a session, because a
// workspace mounted from the host comes back with it. A clone sandbox's
// workspace was made inside the container and went with it: rebuilding one
// hands back an empty sandbox under a name someone recognises, with every
// commit made in it gone and nothing said about it.
func TestEnsureSandboxWillNotRebuildALostClone(t *testing.T) {
	m, err := New(sbx.New(fakeSbx(t)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clone := &Sandbox{ID: "id1", Name: "gone-box", Agent: "claude", Workspace: "/w", Clone: true}
	m.sandboxes[clone.ID] = clone
	m.byName[clone.Name] = clone.ID

	err = m.ensureSandbox(context.Background(), clone)
	if err == nil {
		t.Fatal("the clone sandbox was rebuilt; its workspace is not what it was")
	}
	if !strings.Contains(err.Error(), clone.Name) {
		t.Errorf("err = %v, want it to name the sandbox that is gone", err)
	}

	// The plain kind is still rebuilt: the same folder is mounted back in, so
	// the session that starts in it is the one that would have started in the
	// sandbox that is gone.
	mounted := &Sandbox{ID: "id2", Name: "mounted-box", Agent: "claude", Workspace: "/w"}
	if err := m.ensureSandbox(context.Background(), mounted); err != nil {
		t.Errorf("ensureSandbox of a mounted sandbox: %v", err)
	}
}

func TestBaseTitle(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		kind  Kind
		args  []string
		want  string
	}{
		{"shell ignores the agent", "claude", KindShell, nil, "shell"},
		{"agent with no arguments", "claude", KindAgent, nil, "claude"},
		{"agent with arguments", "claude", KindAgent, []string{"--continue"}, "claude --continue"},
		{"agent unknown", "", KindAgent, nil, "agent"},
		{"the shell agent is not a shell session", "shell", KindAgent, nil, "shell agent"},
		{"a shell session in a shell sandbox stays plain", "shell", KindShell, nil, "shell"},
		{
			"long arguments are trimmed",
			"claude", KindAgent,
			[]string{"--resume", "a-very-long-session-identifier-that-runs-on"},
			"claude --resume a-very-long-session-ide…",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := baseTitle(tt.agent, tt.kind, tt.args)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if len([]rune(got)) > maxTitle {
				t.Errorf("title is %d runes, over the %d cap", len([]rune(got)), maxTitle)
			}
		})
	}
}

// Several agent sessions can share a sandbox, so titles have to tell them
// apart — but only where a number actually earns its place.
func TestUniqueTitleNumbersOnlyCollisions(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}}
	sb := &Sandbox{ID: "sb1", Agent: "claude"}

	add := func(kind Kind, args []string) string {
		title := m.uniqueTitleLocked(sb, kind, args)
		id := newID()
		m.sessions[id] = &Session{ID: id, SandboxID: sb.ID, Kind: kind, Title: title}
		return title
	}

	if got := add(KindAgent, nil); got != "claude" {
		t.Fatalf("first agent = %q, want claude", got)
	}
	if got := add(KindAgent, nil); got != "claude 2" {
		t.Fatalf("second agent = %q, want claude 2", got)
	}
	if got := add(KindAgent, nil); got != "claude 3" {
		t.Fatalf("third agent = %q, want claude 3", got)
	}
	// One of each reads better without numbers on the shell.
	if got := add(KindShell, nil); got != "shell" {
		t.Fatalf("first shell = %q, want shell", got)
	}
	if got := add(KindShell, nil); got != "shell 2" {
		t.Fatalf("second shell = %q, want shell 2", got)
	}
	// Different arguments are context enough on their own.
	if got := add(KindAgent, []string{"--continue"}); got != "claude --continue" {
		t.Fatalf("agent with args = %q", got)
	}
	if got := add(KindAgent, []string{"--continue"}); got != "claude --continue 2" {
		t.Fatalf("second agent with same args = %q", got)
	}
}

// Ending a session gives its number back, so titles stay short instead of
// climbing forever in a long-lived sandbox.
func TestUniqueTitleReusesFreedNumbers(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}}
	sb := &Sandbox{ID: "sb1", Agent: "claude"}

	m.sessions["a"] = &Session{ID: "a", SandboxID: sb.ID, Kind: KindAgent, Title: "claude"}
	m.sessions["b"] = &Session{ID: "b", SandboxID: sb.ID, Kind: KindAgent, Title: "claude 2"}

	delete(m.sessions, "a")
	if got := m.uniqueTitleLocked(sb, KindAgent, nil); got != "claude" {
		t.Fatalf("got %q, want the freed claude", got)
	}
}

// Sandboxes are titled independently: a name taken next door says nothing
// about this one.
func TestUniqueTitleIsPerSandbox(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{
		"a": {ID: "a", SandboxID: "other", Kind: KindAgent, Title: "claude"},
	}}
	if got := m.uniqueTitleLocked(&Sandbox{ID: "sb1", Agent: "claude"}, KindAgent, nil); got != "claude" {
		t.Fatalf("got %q, want claude", got)
	}
}

// A sandbox with no sessions must serialise its session list as an empty
// array, not null: the UI iterates it without a guard.
func TestSandboxViewHasEmptySessionList(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}}
	v := m.viewLocked(&Sandbox{ID: "id1", Name: "box"}, map[string]string{})
	if v.Sessions == nil {
		t.Fatal("sessions should be an empty slice, not nil")
	}
	if v.Status != StatusMissing {
		t.Errorf("status = %q, want %q", v.Status, StatusMissing)
	}
}

// The list is ordered by what was last used, not by what was made first — a
// sandbox someone touched a minute ago belongs above one created an hour ago
// but left idle since.
func TestListSandboxesOrderedByLastActivity(t *testing.T) {
	dir := t.TempDir()
	m, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	old := &Sandbox{ID: "old", Name: "old", Agent: "shell", CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Hour)}
	recent := &Sandbox{ID: "recent", Name: "recent", Agent: "shell", CreatedAt: now.Add(-time.Minute), LastActivityAt: now}
	for _, sb := range []*Sandbox{old, recent} {
		m.sandboxes[sb.ID] = sb
		m.byName[sb.Name] = sb.ID
	}

	out := m.ListSandboxes(context.Background())
	if len(out) != 2 || out[0].ID != "recent" || out[1].ID != "old" {
		t.Fatalf("got order %v, want [recent old]", []string{out[0].ID, out[1].ID})
	}
}

// LastActivityAt has to survive a restart, or the order it produces would
// reset every time the manager does.
func TestLastActivityAtRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}

	activity := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	sb := &Sandbox{ID: "id1", Name: "box", Agent: "shell", CreatedAt: activity, LastActivityAt: activity}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID
	if err := m.saveSandboxes(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetSandbox("id1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if !got.LastActivityAt.Equal(activity) {
		t.Errorf("LastActivityAt = %v, want %v", got.LastActivityAt, activity)
	}
}

// A record written before LastActivityAt existed has no better answer than
// when the sandbox was created — it must not sort as though it had never
// been used at all.
func TestLoadBackfillsMissingLastActivityAt(t *testing.T) {
	dir := t.TempDir()
	createdAt := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	legacy := fmt.Sprintf(`[{"id":"id1","name":"box","agent":"shell","createdAt":%q}]`, createdAt.Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "sandboxes.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.GetSandbox("id1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if !got.LastActivityAt.Equal(createdAt) {
		t.Errorf("LastActivityAt = %v, want backfilled %v", got.LastActivityAt, createdAt)
	}
}

// touchSandboxActivity must be safe to call from many sessions' watchers at
// once, and a value it sets must eventually make it to disk.
func TestTouchSandboxActivityPersists(t *testing.T) {
	dir := t.TempDir()
	m, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}
	sb := &Sandbox{ID: "id1", Name: "box", Agent: "shell", CreatedAt: time.Now()}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID

	before := time.Now()
	m.touchSandboxActivity(sb.ID)
	m.flushActivity()

	reloaded, err := New(sbx.New(""), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetSandbox("id1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if got.LastActivityAt.Before(before) {
		t.Errorf("LastActivityAt = %v, want at or after %v", got.LastActivityAt, before)
	}
}
