package manager

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the process cannot continue safely
	}
	return hex.EncodeToString(b)
}

// sandboxFromSbx derives a sandbox record from one that already exists. sbx
// reports workspaces in mount order, so the first one is the agent's working
// directory and the rest are extra mounts.
func sandboxFromSbx(box sbx.Sandbox) *Sandbox {
	now := time.Now()
	sb := &Sandbox{
		ID:             newID(),
		Name:           box.Name,
		Agent:          box.Agent,
		Adopted:        true,
		CreatedAt:      now,
		LastActivityAt: now,
	}
	if len(box.Workspaces) > 0 {
		sb.Workspace = box.Workspaces[0]
		sb.ExtraWorkspaces = append([]string(nil), box.Workspaces[1:]...)
	}
	return sb
}

// maxTitle keeps a session title readable in the sidebar. Agent arguments are
// the only part that can run long, so that is where the trimming lands.
const maxTitle = 40

// baseTitle names a session after the context it was started in: the agent
// the sandbox runs and the arguments it was given, or plainly "shell". It is
// not unique on its own — uniqueTitleLocked numbers collisions.
func baseTitle(agent string, kind Kind, agentArgs []string) string {
	if kind == KindShell {
		return "shell"
	}
	name := agent
	if name == "" {
		name = "agent"
	}
	// A sandbox built for the "shell" agent would otherwise title its agent
	// sessions exactly like its shell sessions, and they are not the same
	// thing: one is the sandbox's agent under "sbx run", the other an extra
	// terminal under "sbx exec".
	if name == "shell" {
		name = "shell agent"
	}
	if len(agentArgs) == 0 {
		return name
	}
	return truncate(name+" "+strings.Join(agentArgs, " "), maxTitle)
}

// truncate shortens s to at most n characters, marking that it was cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9.+-]+`)

// defaultName mirrors sbx's own "<agent>-<workdir>" default.
func defaultName(agent, workspace string) string {
	base := filepath.Base(strings.TrimSuffix(workspace, string(os.PathSeparator)))
	base = unsafeName.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" || base == "." {
		return agent
	}
	return agent + "-" + base
}

// DefaultWorktreeSandboxName names a worktree's sandbox after the project it
// belongs to plus a random slug, rather than the branch it was made for: a
// branch name can be long, nested with slashes, or shared by worktrees made
// from it more than once, none of which make a good — or unique — sandbox
// name on their own.
func DefaultWorktreeSandboxName(projectName string) string {
	base := unsafeName.ReplaceAllString(strings.ToLower(projectName), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "project"
	}
	return base + "-" + randomSlug()
}

// randomSlug is a short random lowercase-hex string, enough to keep sandbox
// names made moments apart from colliding without meaning anything itself.
func randomSlug() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the process cannot continue safely
	}
	return hex.EncodeToString(b)
}

// resolveWorkspace expands and validates a host directory to mount into the
// sandbox.
func resolveWorkspace(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("workspace path is required")
	}
	abs, err := expand(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace %q is not accessible: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", abs)
	}
	return abs, nil
}

// resolveExtraWorkspace handles the optional ":ro" suffix sbx uses for
// read-only mounts.
func resolveExtraWorkspace(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty extra workspace")
	}
	readOnly := strings.HasSuffix(p, ":ro")
	path := strings.TrimSuffix(p, ":ro")
	abs, err := resolveWorkspace(path)
	if err != nil {
		return "", err
	}
	if readOnly {
		return abs + ":ro", nil
	}
	return abs, nil
}

func expand(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return filepath.Abs(p)
}

// agentEnv gives the session a colour-capable terminal; sbx inherits the rest
// of the manager's environment (Docker context, credentials helpers, ...).
func agentEnv() []string {
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}

// dims clamps terminal dimensions to something a PTY will accept.
func dims(cols, rows uint16) (uint16, uint16) {
	if cols < 20 || cols > 500 {
		cols = 120
	}
	if rows < 5 || rows > 300 {
		rows = 32
	}
	return cols, rows
}
