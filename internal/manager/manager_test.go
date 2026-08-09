package manager

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
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
	if err := m.save(); err != nil {
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
