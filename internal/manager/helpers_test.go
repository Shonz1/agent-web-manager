package manager

import (
	"slices"
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

// A sandbox can report no workspaces at all; that must not panic or invent a
// mount the sandbox does not have.
func TestSandboxFromSbxWithoutWorkspaces(t *testing.T) {
	sb := sandboxFromSbx(sbx.Sandbox{Name: "bare", Agent: "shell"})
	if sb.Workspace != "" || len(sb.ExtraWorkspaces) != 0 {
		t.Fatalf("got workspace %q extras %q", sb.Workspace, sb.ExtraWorkspaces)
	}
}
