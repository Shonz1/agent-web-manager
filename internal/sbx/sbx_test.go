package sbx

import (
	"slices"
	"testing"
)

func TestCreateArgs(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "minimal",
			got:  CreateArgs("box", "claude", "/w", nil, nil),
			want: []string{"create", "--name", "box", "claude", "/w"},
		},
		{
			name: "extra workspaces follow the primary one",
			got:  CreateArgs("box", "claude", "/w", []string{"/docs:ro"}, nil),
			want: []string{"create", "--name", "box", "claude", "/w", "/docs:ro"},
		},
		{
			// "sbx create" routes to a per-agent subcommand, so every flag
			// has to be parsed before the agent name is seen.
			name: "published ports precede the agent",
			got:  CreateArgs("box", "shell", "/w", nil, []string{"3000:3000", "8080"}),
			want: []string{"create", "--name", "box", "--publish", "3000:3000", "--publish", "8080", "shell", "/w"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Equal(tt.got, tt.want) {
				t.Fatalf("got %q\nwant %q", tt.got, tt.want)
			}
		})
	}
}

func TestAttachArgsOmitsAgentAndWorkspace(t *testing.T) {
	got := AttachArgs("box", []string{"--continue"})
	want := []string{"run", "--name", "box", "--", "--continue"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestShellArgs(t *testing.T) {
	got := ShellArgs("box")
	want := []string{"exec", "-it", "box", "bash"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"claude-my-app", "a.b+c-d", "box1"}
	invalid := []string{"", "has space", "slash/name", "under_score", "quote'; rm -rf"}

	for _, n := range valid {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestValidAgent(t *testing.T) {
	if !ValidAgent("claude") {
		t.Error("claude should be a valid agent")
	}
	if ValidAgent("not-an-agent") {
		t.Error("unknown agent should be rejected")
	}
}
