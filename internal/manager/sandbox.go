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
	// Adopted marks a sandbox this manager did not create: its agent and
	// workspaces were read back from the sandbox itself, so it cannot be
	// faithfully recreated once it is gone.
	Adopted bool `json:"adopted,omitempty"`
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
