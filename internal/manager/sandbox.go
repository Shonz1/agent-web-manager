package manager

import "time"

// Sandbox is the manager's persisted record of one sbx sandbox.
//
// It holds only what the manager has to remember in order to recreate the
// sandbox: everything mutable lives elsewhere. Whether the container is up is
// sbx's business, and which terminals are attached to it is the session
// table's.
type Sandbox struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Agent           string    `json:"agent"`
	Workspace       string    `json:"workspace"`
	ExtraWorkspaces []string  `json:"extraWorkspaces,omitempty"`
	Publish         []string  `json:"publish,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	// LastActivityAt is the last time any session in this sandbox did
	// something — set on creation and bumped as its sessions run, so the
	// sandbox list can be ordered by what was last used rather than by when it
	// was made, and that order survives a restart.
	LastActivityAt time.Time `json:"lastActivityAt"`
	// Adopted marks a sandbox this manager did not create: its agent and
	// workspaces were read back from the sandbox itself, so it cannot be
	// faithfully recreated once it is gone.
	Adopted bool `json:"adopted,omitempty"`

	// ProjectID is the project this sandbox belongs to, or empty for one made
	// directly - by "sbx create" outside this manager, or through the
	// advanced sandbox screen rather than a project.
	ProjectID string `json:"projectId,omitempty"`
	// IsWorktree marks a sandbox mounted on a worktree of a project's
	// repository rather than on the project's folder itself. A project has at
	// most one sandbox that is not one of these; see EnsureProjectSandbox.
	IsWorktree bool `json:"isWorktree,omitempty"`
	// RepoRoot is the main working tree the IsWorktree checkout belongs to, so
	// its worktree can be removed from git when this sandbox is deleted along
	// with its project. Empty when IsWorktree is false.
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
