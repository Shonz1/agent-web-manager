package manager

import (
	"slices"
	"strings"
	"testing"
)

const testConvID = "f8131200-af24-483a-9e49-b471fc5d60ea"

func TestClaudePin(t *testing.T) {
	tests := []struct {
		name      string
		agentArgs []string
		wantID    string // "" means "any generated ID"; "-" means none at all
		wantArgs  []string
	}{
		{
			name:     "a plain session is pinned",
			wantArgs: []string{"--session-id"},
		},
		{
			name:      "unrelated arguments are left alone",
			agentArgs: []string{"--model", "opus"},
			wantArgs:  []string{"--session-id"},
		},
		{
			// claude rejects --session-id alongside these unless the session
			// is forked, which would continue the conversation elsewhere.
			name:      "continue is left unpinned",
			agentArgs: []string{"--continue"},
			wantID:    "-",
		},
		{
			name:      "short continue is left unpinned",
			agentArgs: []string{"-c"},
			wantID:    "-",
		},
		{
			name:      "resume is left unpinned",
			agentArgs: []string{"--resume", testConvID},
			wantID:    "-",
		},
		{
			name:      "a caller's own ID is followed, not replaced",
			agentArgs: []string{"--session-id", testConvID},
			wantID:    testConvID,
		},
		{
			name:      "a caller's own ID is followed in --flag=value form",
			agentArgs: []string{"--session-id=" + testConvID},
			wantID:    testConvID,
		},
		{
			// Anything that is not a conversation ID cannot name a
			// transcript, and must never reach the shell that reads one.
			name:      "an unusable ID yields no watch",
			agentArgs: []string{"--session-id", "$(touch /tmp/pwned)"},
			wantID:    "-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin := claudePin(tt.agentArgs)

			switch tt.wantID {
			case "-":
				if pin.id != "" {
					t.Errorf("id = %q, want none", pin.id)
				}
			case "":
				if !convIDRE.MatchString(pin.id) {
					t.Errorf("id = %q, want a generated conversation ID", pin.id)
				}
			default:
				if pin.id != tt.wantID {
					t.Errorf("id = %q, want %q", pin.id, tt.wantID)
				}
			}
			if len(tt.wantArgs) == 0 && len(pin.args) != 0 {
				t.Errorf("args = %q, want none", pin.args)
			}
			if len(tt.wantArgs) > 0 {
				if len(pin.args) != 2 || pin.args[0] != tt.wantArgs[0] || pin.args[1] != pin.id {
					t.Errorf("args = %q, want %q with the pinned ID", pin.args, tt.wantArgs[0])
				}
			}
		})
	}
}

// A pinned session has to carry the flag through to the agent, and an agent
// that cannot be pinned has to be left exactly as the caller wrote it.
func TestSessionArgvPinning(t *testing.T) {
	claude := &Sandbox{Name: "cc-claude", Agent: "claude"}

	argv, convID := sessionArgv(claude, KindAgent, []string{"--model", "opus"})
	if convID == "" {
		t.Fatal("a claude session should be pinned to a conversation")
	}
	want := []string{"run", "--name", "cc-claude", "--", "--session-id", convID, "--model", "opus"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}

	argv, convID = sessionArgv(claude, KindAgent, []string{"--continue"})
	if convID != "" {
		t.Errorf("a continued session should not be pinned, got %q", convID)
	}
	want = []string{"run", "--name", "cc-claude", "--", "--continue"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}

	// codex is read positionally: it has no flag to pin a conversation, so
	// nothing may be added to what the caller asked for.
	codex := &Sandbox{Name: "cx", Agent: "codex"}
	argv, convID = sessionArgv(codex, KindAgent, []string{"--full-auto"})
	if convID != "" {
		t.Errorf("codex cannot be pinned, got %q", convID)
	}
	want = []string{"run", "--name", "cx", "--", "--full-auto"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}

	// An agent nobody knows how to read is left alone entirely.
	other := &Sandbox{Name: "kr", Agent: "kiro"}
	if argv, convID = sessionArgv(other, KindAgent, nil); convID != "" {
		t.Errorf("unknown agent should not be pinned, got %q", convID)
	}
	if want := []string{"run", "--name", "kr"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}

	// A shell is not an agent and has no conversation to name.
	argv, convID = sessionArgv(claude, KindShell, nil)
	if convID != "" {
		t.Errorf("shell session should not be pinned, got %q", convID)
	}
	if want := []string{"exec", "-it", "cc-claude", "bash"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// Pinning must not mutate the caller's slice: the arguments are kept on the
// session and used again on every restart.
func TestSessionArgvLeavesCallerArgsAlone(t *testing.T) {
	sb := &Sandbox{Name: "cc-claude", Agent: "claude"}
	args := []string{"--model", "opus"}

	sessionArgv(sb, KindAgent, args)

	if !slices.Equal(args, []string{"--model", "opus"}) {
		t.Errorf("caller arguments were modified: %q", args)
	}
}

func TestParseClaudeTitle(t *testing.T) {
	const record = `{"type":"ai-title","aiTitle":"Create schema-based query service","sessionId":"` + testConvID + `"}`

	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "a title record",
			out:  record + "\n",
			want: "Create schema-based query service",
		},
		{
			// The transcript keeps every title it ever generated, and the
			// grep that finds them is not guaranteed to return just one.
			name: "the last title wins",
			out:  record + "\n" + `{"type":"ai-title","aiTitle":"Rename the query service"}` + "\n",
			want: "Rename the query service",
		},
		{
			name: "sbx's own chatter is ignored",
			out:  "Sandbox cc-claude started successfully\n" + record + "\n",
			want: "Create schema-based query service",
		},
		{
			name: "a conversation with no title yet",
			out:  "",
		},
		{
			name: "some other record",
			out:  `{"type":"mode","mode":"normal"}` + "\n",
		},
		{
			name: "truncated output",
			out:  `{"type":"ai-title","aiTit`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseClaudeTitle([]byte(tt.out), ""); got != tt.want {
				t.Errorf("parseClaudeTitle = %q, want %q", got, tt.want)
			}
		})
	}
}

// Where codex records the opening prompt has moved between versions. Both
// shapes here are real rollouts' opening lines, trimmed to what the script
// sends back: the session_meta, and the candidate user messages.
const (
	// codex 0.147: the prompt arrives as a completed UserMessage item, and
	// the first "user" record is an environment briefing codex wrote itself.
	codexRolloutNew = `{"timestamp":"2026-08-09T17:52:19.788Z","ordinal":0,"type":"session_meta","payload":{"session_id":"019fe7a7-1713-7b62-b7b5-df16b1b8af0c","cwd":"/w/app","originator":"codex-tui","cli_version":"0.147.0","source":"cli"}}
{"ordinal":5,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/w/app</cwd>\n  <shell>bash</shell>\n</environment_context>"}]}}
{"ordinal":8,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Summarize recent commits"}]}}
{"ordinal":9,"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"Summarize recent commits"}]}}}
`
	// codex 0.65: a plain user_message event.
	codexRolloutOld = `{"timestamp":"2025-12-06T21:45:42.360Z","type":"session_meta","payload":{"id":"019af5a0-b8be-7390-9874-7659b6f90f75","cwd":"/w/app","cli_version":"0.65.0","source":"cli"}}
{"timestamp":"2025-12-06T21:45:42.360Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/w/app</cwd>\n</environment_context>"}]}}
{"timestamp":"2025-12-06T21:45:42.362Z","type":"event_msg","payload":{"type":"user_message","message":"Write unit tests for PersonArchiveListUseCase","images":[]}}
`
)

func TestParseCodexTitle(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		workspace string
		want      string
	}{
		{
			name:      "the prompt the session opened with",
			out:       codexRolloutNew,
			workspace: "/w/app",
			want:      "Summarize recent commits",
		},
		{
			name:      "the prompt in an older codex's rollout",
			out:       codexRolloutOld,
			workspace: "/w/app",
			want:      "Write unit tests for PersonArchiveListUseCase",
		},
		{
			// A rollout from somewhere else in the container is some other
			// session's, and its prompt would be a lie about this one.
			name:      "a rollout for another workspace is refused",
			out:       codexRolloutNew,
			workspace: "/w/other",
		},
		{
			name:      "a session that has not been prompted yet",
			out:       strings.SplitAfter(codexRolloutNew, "\n")[0],
			workspace: "/w/app",
		},
		{
			// The environment briefing is the only user-role record such a
			// session has, and it says nothing about the work.
			name: "a briefing is not a prompt",
			out: strings.Join(strings.SplitAfter(codexRolloutNew, "\n")[:2], "") +
				`{"ordinal":9,"type":"event_msg","payload":{"type":"task_started"}}`,
			workspace: "/w/app",
		},
		{
			name:      "no rollout at all",
			out:       "",
			workspace: "/w/app",
		},
		{
			// Without a session_meta there is nothing that ties the rollout
			// to this sandbox, so there is nothing to show.
			name:      "an unattributable rollout is refused",
			out:       `{"type":"event_msg","payload":{"type":"user_message","message":"Do a thing"}}`,
			workspace: "/w/app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCodexTitle([]byte(tt.out), tt.workspace); got != tt.want {
				t.Errorf("parseCodexTitle = %q, want %q", got, tt.want)
			}
		})
	}
}

// The shape here is what "opencode session list --format json" prints.
const opencodeSessions = `[
  {"id":"ses_a","title":"Refactor the sync settings","updated":200,"created":100,"directory":"/w/app"},
  {"id":"ses_b","title":"An older thing","updated":150,"created":50,"directory":"/w/app"},
  {"id":"ses_c","title":"Something in another checkout","updated":300,"created":90,"directory":"/w/other"}
]`

func TestParseOpencodeTitle(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		workspace string
		want      string
	}{
		{
			name:      "the newest session in this workspace",
			out:       opencodeSessions,
			workspace: "/w/app",
			want:      "Refactor the sync settings",
		},
		{
			name:      "sbx's own chatter is ignored",
			out:       "Sandbox oc started successfully\n" + opencodeSessions,
			workspace: "/w/app",
			want:      "Refactor the sync settings",
		},
		{
			// Until opencode has titled a session it holds a placeholder,
			// which says less than the name the manager gave it.
			name:      "an untitled session is skipped",
			out:       `[{"id":"ses_a","title":"New session - 2026-08-01T16:33:43.917Z","updated":9,"directory":"/w/app"}]`,
			workspace: "/w/app",
		},
		{
			name:      "no sessions in this workspace",
			out:       opencodeSessions,
			workspace: "/w/elsewhere",
		},
		{
			name:      "no sessions at all",
			out:       "[]",
			workspace: "/w/app",
		},
		{
			name:      "output that is not a session list",
			out:       "opencode: command not found",
			workspace: "/w/app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseOpencodeTitle([]byte(tt.out), tt.workspace); got != tt.want {
				t.Errorf("parseOpencodeTitle = %q, want %q", got, tt.want)
			}
		})
	}
}

// Agents that store a prompt rather than a summary can hand back something
// arbitrarily long, and a sidebar has to survive it.
func TestSummarize(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "a short title is untouched", title: "Fix the sync", want: "Fix the sync"},
		{
			name:  "only the first line is kept",
			title: "\n  Fix the sync bug  \nand then a lot more explanation\n",
			want:  "Fix the sync bug",
		},
		{
			name:  "a long line is cut",
			title: strings.Repeat("a", 200),
			want:  strings.Repeat("a", maxAgentTitle-1) + "…",
		},
		{name: "nothing at all", title: "\n \n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarize(tt.title)
			if got != tt.want {
				t.Errorf("summarize = %q, want %q", got, tt.want)
			}
			if len([]rune(got)) > maxAgentTitle {
				t.Errorf("summarize returned %d runes, over the %d cap", len([]rune(got)), maxAgentTitle)
			}
		})
	}
}

// A session read by position must not take a title while another agent
// session could just as well own it.
func TestSoleAgentSession(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}}
	add := func(id, sandboxID string, kind Kind, status Status) *Session {
		s := newSession(id, sandboxID, "sb", kind, nil, id)
		s.status = status
		m.sessions[id] = s
		return s
	}

	first := add("a", "sb1", KindAgent, StatusRunning)
	if !m.soleAgentSession(first) {
		t.Error("a lone agent session should be readable")
	}

	// A shell shares the sandbox but has no conversation of its own.
	add("shell", "sb1", KindShell, StatusRunning)
	// So does an agent session in a different sandbox.
	add("elsewhere", "sb2", KindAgent, StatusRunning)
	// And one that has already exited cannot be writing titles.
	add("dead", "sb1", KindAgent, StatusExited)
	if !m.soleAgentSession(first) {
		t.Error("only live agent sessions in the same sandbox should count")
	}

	second := add("b", "sb1", KindAgent, StatusRunning)
	if m.soleAgentSession(first) || m.soleAgentSession(second) {
		t.Error("two live agent sessions in one sandbox are not attributable")
	}
}

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newUUID()
		if !convIDRE.MatchString(id) {
			t.Fatalf("newUUID produced %q, which claude will not accept", id)
		}
		if seen[id] {
			t.Fatalf("newUUID repeated %q", id)
		}
		seen[id] = true
	}
}
