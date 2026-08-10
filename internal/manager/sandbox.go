package manager

import "time"

// Sandbox is the manager's persisted record of one sbx sandbox.
//
// It holds only what the manager has to remember in order to recreate the
// sandbox: everything mutable lives elsewhere. Whether the container is up is
// sbx's business, and which terminals are attached to it is the session
// table's.
type Sandbox struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Agent           string   `json:"agent"`
	Workspace       string   `json:"workspace"`
	ExtraWorkspaces []string `json:"extraWorkspaces,omitempty"`
	Publish         []string `json:"publish,omitempty"`
	// Kits are the sbx kits this sandbox was created with, by name. Kept
	// because they cannot be added afterwards: a sandbox sbx has lost is
	// rebuilt with them, and until then this is the only remaining record that
	// the session running in it was given more than the agent image.
	Kits      []string  `json:"kits,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// LastActivityAt is the last time a person used this sandbox — set on
	// creation and bumped whenever a session of its is started or typed at, so
	// the sandbox list can be ordered by what was last used rather than by when
	// it was made, and that order survives a restart. Work an agent does on its
	// own does not move it: a list ordered by that would rearrange itself under
	// whoever is reading it.
	LastActivityAt time.Time `json:"lastActivityAt"`
	// Adopted marks a sandbox this manager did not create: its agent and
	// workspaces were read back from the sandbox itself, so it cannot be
	// faithfully recreated once it is gone.
	Adopted bool `json:"adopted,omitempty"`

	// ProjectID is the project this sandbox belongs to. Empty only for a
	// sandbox left over from before every sandbox this manager creates
	// belonged to one, or made directly by "sbx create" outside this manager.
	ProjectID string `json:"projectId,omitempty"`
	// IsBase marks a project's base sandbox: the one made with the project
	// itself, mounted on its folder, and never used for anything. Nothing is
	// started in it and it cannot be deleted on its own — it is there to be
	// the thing every session sandbox is cloned from, so what a
	// session inherits is a settled sandbox rather than whichever one
	// happened to be made first. A project has exactly one; see
	// EnsureBaseSandbox.
	IsBase bool `json:"isBase,omitempty"`
	// Clone marks a sandbox created in sbx's clone mode: its workspace is a
	// standalone git clone of the host checkout rather than the checkout
	// itself, so nothing it does reaches the host until someone fetches it.
	// This is what lets a project run several sessions on one folder at once
	// without giving each a worktree.
	Clone bool `json:"clone,omitempty"`
	// IsWorktree marks a sandbox mounted on a worktree of a project's
	// repository rather than on the project's folder itself.
	IsWorktree bool `json:"isWorktree,omitempty"`
	// RepoRoot is the main working tree the IsWorktree checkout belongs to, so
	// its worktree can be removed from git when this sandbox is deleted —
	// on its own or along with its project. Empty when IsWorktree is false.
	RepoRoot string `json:"repoRoot,omitempty"`
}

// SandboxView is the JSON-facing snapshot of a sandbox: the persisted record,
// the live container status from sbx, and the sessions running inside it.
type SandboxView struct {
	Sandbox
	Status   string        `json:"status"`
	Sessions []SessionView `json:"sessions"`
}

// StatusMissing is the sandbox status reported when sbx no longer lists a
// sandbox the manager still has a record of.
const StatusMissing = "missing"
