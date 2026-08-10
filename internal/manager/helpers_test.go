package manager

import (
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// Nothing outside this package names a session's sandbox any more, so this
// name is the only one there will be: it has to be valid, derived from the
// project rather than the branch, and different every time.
func TestDefaultProjectSandboxName(t *testing.T) {
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
		got := DefaultProjectSandboxName(tc.project)
		if !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("DefaultProjectSandboxName(%q) = %q, want the prefix %q", tc.project, got, tc.prefix)
		}
		if !sbx.ValidName(got) {
			t.Errorf("DefaultProjectSandboxName(%q) = %q, which sbx will not accept", tc.project, got)
		}
	}

	// Two worktrees of one project, made moments apart, must not collide —
	// which is the whole reason the name carries a slug rather than a branch.
	seen := make(map[string]bool)
	for range 50 {
		name := DefaultProjectSandboxName("demo")
		if seen[name] {
			t.Fatalf("%q came back twice", name)
		}
		seen[name] = true
	}
}
