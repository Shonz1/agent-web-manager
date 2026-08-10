package manager

import (
	"slices"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

func TestSandboxFromSbx(t *testing.T) {
	box := sbx.Sandbox{
		Name:       "cc-claude",
		Agent:      "claude",
		Status:     "running",
		Workspaces: []string{"/w/app", "/w/docs:ro"},
	}
	sb := sandboxFromSbx(box)

	if sb.Name != "cc-claude" || sb.Agent != "claude" {
		t.Fatalf("got name %q agent %q", sb.Name, sb.Agent)
	}
	if sb.Workspace != "/w/app" {
		t.Errorf("workspace = %q, want /w/app", sb.Workspace)
	}
	if !slices.Equal(sb.ExtraWorkspaces, []string{"/w/docs:ro"}) {
		t.Errorf("extra workspaces = %q", sb.ExtraWorkspaces)
	}
	if !sb.Adopted {
		t.Error("sandbox should be marked adopted")
	}
	if sb.ID == "" || sb.CreatedAt.IsZero() {
		t.Error("sandbox should carry an ID and a creation time")
	}
}

// Nothing outside this package names a worktree sandbox any more, so this
// name is the only one there will be: it has to be valid, derived from the
// project rather than the branch, and different every time.
func TestDefaultWorktreeSandboxName(t *testing.T) {
	for _, tc := range []struct {
		project string
		prefix  string
	}{
		{"Demo", "demo-"},
		{"My App", "my-app-"},
		{"agent/web manager", "agent-web-manager-"},
		{"", "project-"},
		{"///", "project-"},
	} {
		got := DefaultWorktreeSandboxName(tc.project)
		if !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("DefaultWorktreeSandboxName(%q) = %q, want the prefix %q", tc.project, got, tc.prefix)
		}
		if !sbx.ValidName(got) {
			t.Errorf("DefaultWorktreeSandboxName(%q) = %q, which sbx will not accept", tc.project, got)
		}
	}

	// Two worktrees of one project, made moments apart, must not collide —
	// which is the whole reason the name carries a slug rather than a branch.
	seen := make(map[string]bool)
	for range 50 {
		name := DefaultWorktreeSandboxName("demo")
		if seen[name] {
			t.Fatalf("%q came back twice", name)
		}
		seen[name] = true
	}
}

// A sandbox can report no workspaces at all; that must not panic or invent a
// mount the sandbox does not have.
func TestSandboxFromSbxWithoutWorkspaces(t *testing.T) {
	sb := sandboxFromSbx(sbx.Sandbox{Name: "bare", Agent: "shell"})
	if sb.Workspace != "" || len(sb.ExtraWorkspaces) != 0 {
		t.Fatalf("got workspace %q extras %q", sb.Workspace, sb.ExtraWorkspaces)
	}
}
