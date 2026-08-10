// Package sbx wraps the Docker Sandboxes ("sbx") CLI.
package sbx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Agents that "sbx run" accepts as its first positional argument.
var Agents = []string{
	"claude", "codex", "copilot", "cursor", "docker-agent",
	"droid", "gemini", "kiro", "opencode", "shell",
}

// Sandbox is one entry of "sbx ls --json".
type Sandbox struct {
	Name       string   `json:"name"`
	ID         string   `json:"id"`
	Agent      string   `json:"agent"`
	Status     string   `json:"status"`
	Workspaces []string `json:"workspaces"`
}

type listResponse struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

// Client runs sbx commands. The zero value uses "sbx" from PATH.
type Client struct {
	Bin string
}

func New(bin string) *Client {
	if bin == "" {
		bin = "sbx"
	}
	return &Client{Bin: bin}
}

// Available reports whether the sbx binary can be found and executed.
func (c *Client) Available(ctx context.Context) error {
	if _, err := exec.LookPath(c.Bin); err != nil {
		return fmt.Errorf("%q not found in PATH: %w", c.Bin, err)
	}
	if _, err := c.output(ctx, "version"); err != nil {
		return fmt.Errorf("%q is not runnable: %w", c.Bin, err)
	}
	return nil
}

// List returns every sandbox sbx knows about, including ones this manager
// did not create.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	out, err := c.output(ctx, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var resp listResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse sbx ls output: %w", err)
	}
	return resp.Sandboxes, nil
}

// Get returns the sandbox with the given name, or ok=false if it is gone.
func (c *Client) Get(ctx context.Context, name string) (Sandbox, bool, error) {
	boxes, err := c.List(ctx)
	if err != nil {
		return Sandbox{}, false, err
	}
	for _, b := range boxes {
		if b.Name == name {
			return b, true, nil
		}
	}
	return Sandbox{}, false, nil
}

// Stop stops a sandbox without removing it.
func (c *Client) Stop(ctx context.Context, name string) error {
	_, err := c.output(ctx, "stop", name)
	return err
}

// Remove deletes a sandbox and all of its resources. This is irreversible.
func (c *Client) Remove(ctx context.Context, name string) error {
	_, err := c.output(ctx, "rm", "--force", name)
	return err
}

// CreateOptions describes the sandbox "sbx create" should make. It is a
// struct rather than a parameter list because the interesting part is which
// of the optional pieces are set, and a call site naming them reads far
// better than one counting nils.
type CreateOptions struct {
	Name            string
	Agent           string
	Workspace       string
	ExtraWorkspaces []string
	Publish         []string
	// Clone asks for a sandbox whose workspace is a standalone git clone of
	// the host checkout, made when the sandbox starts, rather than the host
	// checkout itself bind-mounted in. Nothing the agent does inside reaches
	// the host until someone fetches from the sandbox, which is what makes
	// several sandboxes on one folder safe to run at once.
	Clone bool
}

// Create makes a sandbox and returns once it exists, without attaching to it.
// Sessions are started separately, with AttachArgs or ShellArgs.
func (c *Client) Create(ctx context.Context, opts CreateOptions) error {
	_, err := c.output(ctx, CreateArgs(opts)...)
	return err
}

// CreateArgs builds the argv for "sbx create": a sandbox with no session
// running in it yet. The flags come before the agent because "sbx create"
// dispatches to a per-agent subcommand once it sees the agent name.
func CreateArgs(opts CreateOptions) []string {
	args := []string{"create", "--name", opts.Name}
	if opts.Clone {
		args = append(args, "--clone")
	}
	for _, p := range opts.Publish {
		args = append(args, "--publish", p)
	}
	args = append(args, opts.Agent)
	if opts.Workspace != "" {
		args = append(args, opts.Workspace)
	}
	return append(args, opts.ExtraWorkspaces...)
}

// AttachArgs builds the argv for an agent session: "sbx run" against a
// sandbox that already exists reads the agent and workspaces back from the
// sandbox's own spec, so nothing about it is re-asserted here.
func AttachArgs(name string, agentArgs []string) []string {
	args := []string{"run", "--name", name}
	if len(agentArgs) > 0 {
		args = append(args, "--")
		args = append(args, agentArgs...)
	}
	return args
}

// Exec runs a command inside a sandbox and returns its stdout. It is the
// non-interactive counterpart of ShellArgs: no TTY and no attachment, for
// reading something back out of a sandbox that is already running.
func (c *Client) Exec(ctx context.Context, name string, argv ...string) ([]byte, error) {
	return c.output(ctx, append([]string{"exec", name}, argv...)...)
}

// ShellArgs builds the argv for a shell session: an interactive shell beside
// whatever else is running in the sandbox. "sbx exec" starts the sandbox
// first if it is stopped.
func ShellArgs(name string) []string {
	return []string{"exec", "-it", name, "bash"}
}

func (c *Client) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("sbx %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("sbx %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// sbx restricts sandbox names to letters, numbers, hyphens, periods, plus and
// minus signs.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)

// ValidName reports whether name is acceptable to sbx.
func ValidName(name string) bool {
	return name != "" && len(name) <= 64 && nameRE.MatchString(name)
}

// ValidAgent reports whether agent is one of the agents sbx can launch.
func ValidAgent(agent string) bool {
	for _, a := range Agents {
		if a == agent {
			return true
		}
	}
	return false
}
