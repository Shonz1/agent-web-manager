package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Agents name their own work, and that name says what a session is doing in a
// way "claude 2" never can. Each agent records it somewhere else and calls it
// something else, so what can be read back — and how a session is matched to
// its own record — is per agent. None of it comes from sbx, which has no
// concept of a session at all: the manager reads it out of the container with
// "sbx exec".
const (
	agentClaude   = "claude"
	agentCodex    = "codex"
	agentOpencode = "opencode"
)

const (
	// titlePoll is how often a live session is asked for its title. Every
	// poll is an "sbx exec" into the container, so this is deliberately
	// unhurried: a title only moves when the conversation does.
	titlePoll = 15 * time.Second
	// titleTimeout bounds one such poll, so a wedged container cannot leave
	// the watcher hanging until the session ends.
	titleTimeout = 10 * time.Second
	// maxAgentTitle keeps a title to something a sidebar can show. Agents
	// that hand back a whole prompt rather than a summary need it.
	maxAgentTitle = 60
)

// convPin is a conversation an agent was told to run under.
type convPin struct {
	// id identifies the conversation, or is empty when this session has none
	// that can be followed.
	id string
	// args are the flags that pin it, to be passed ahead of the caller's own.
	// Empty when the caller pinned the conversation themselves.
	args []string
}

// titleReader knows how one agent records what it is working on.
type titleReader struct {
	// pin decides which conversation the agent is started under. Agents that
	// cannot be told which one to use have no pin, and are matched to their
	// record by position instead — which is only sound while the sandbox has
	// a single agent session in it, so that is the only time they are read.
	pin func(agentArgs []string) convPin
	// script prints whatever the title has to be read out of. It is run with
	// sh inside the sandbox, and is given only the pinned conversation ID:
	// nothing else about a session reaches a shell.
	script func(convID string) string
	// parse pulls the title out of that output, and returns "" when there is
	// no title yet or when what came back belongs to another conversation.
	parse func(out []byte, workspace string) string
}

// titleReaders holds the agents whose record of themselves this manager knows
// how to read. An agent that is not here simply keeps its plain name.
var titleReaders = map[string]titleReader{
	agentClaude:   {pin: claudePin, script: claudeScript, parse: parseClaudeTitle},
	agentCodex:    {script: codexScript, parse: parseCodexTitle},
	agentOpencode: {script: opencodeScript, parse: parseOpencodeTitle},
}

// convIDRE matches the conversation IDs claude accepts. It also guards the
// only place an ID reaches a shell, so an ID that came in from the API cannot
// carry anything else with it.
var convIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// --- claude ---
//
// claude generates a title from the conversation and rewrites it as the work
// moves on, into the transcript it keeps per conversation. Pinning the
// conversation up front is what makes that transcript findable: a sandbox
// running several agents gives no other way to tell whose is whose.

func claudePin(agentArgs []string) convPin {
	for i, arg := range agentArgs {
		flag, value, hasValue := strings.Cut(arg, "=")
		switch flag {
		case "--session-id":
			// The caller pinned the conversation themselves. Follow their ID
			// rather than fighting them for the flag.
			if !hasValue && i+1 < len(agentArgs) {
				value = agentArgs[i+1]
			}
			if !convIDRE.MatchString(value) {
				return convPin{}
			}
			return convPin{id: value}
		case "-c", "--continue", "-r", "--resume":
			// claude rejects --session-id alongside these unless the session
			// is forked, and forking would quietly continue the conversation
			// somewhere else. Such a session keeps its plain title.
			return convPin{}
		}
	}
	id := newUUID()
	return convPin{id: id, args: []string{"--session-id", id}}
}

func claudeScript(convID string) string {
	// The project directory is named after the workspace path, mangled; the
	// glob avoids having to reproduce that mangling here. A glob that matches
	// nothing leaves grep with a missing file, which the pipeline swallows.
	return fmt.Sprintf(
		`grep -h '"type":"ai-title"' "$HOME"/.claude/projects/*/%s.jsonl 2>/dev/null | tail -1`,
		convID,
	)
}

// parseClaudeTitle takes the last title in the transcript: the record is
// appended again every time the title is regenerated. The conversation is
// already pinned, so nothing here has to be matched to a workspace.
func parseClaudeTitle(out []byte, _ string) string {
	var title string
	for _, line := range jsonLines(out) {
		var rec struct {
			Type    string `json:"type"`
			AITitle string `json:"aiTitle"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type == "ai-title" && rec.AITitle != "" {
			title = rec.AITitle
		}
	}
	return title
}

// --- codex ---
//
// codex does not generate a title. What it shows for a session, and stores as
// one, is the prompt the session opened with, so that is what is read here.
// There is no way to tell codex which conversation to be, so the newest
// rollout is taken as this session's — sound only while the sandbox has one
// agent session in it, which is the only time an unpinned reader runs.

func codexScript(string) string {
	// The opening records carry developer preamble that runs to tens of
	// kilobytes, so the prompt is picked out inside the sandbox rather than
	// shipped back whole: the first line is the session_meta that says whose
	// rollout this is, and the rest are candidate user messages.
	return `f=$(ls -t "$HOME"/.codex/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -1); ` +
		`[ -n "$f" ] && { head -n 1 "$f"; ` +
		`grep -m 6 -e '"type":"UserMessage"' -e '"type":"user_message"' -e '"role":"user"' "$f"; ` +
		`} | head -c 262144`
}

// codexRecord is as much of a rollout record as it takes to find the prompt a
// session opened with. Where that prompt lives has moved between codex
// versions, so every shape it has had is read.
type codexRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type string `json:"type"`
		Cwd  string `json:"cwd"`
		// A "message" response item: the prompt as it was sent to the model.
		Role    string      `json:"role"`
		Content []codexText `json:"content"`
		// An "item_completed" event, which is where codex 0.147 puts it.
		Item struct {
			Type    string      `json:"type"`
			Content []codexText `json:"content"`
		} `json:"item"`
		// A "user_message" event, which is where older codex put it.
		Message string `json:"message"`
	} `json:"payload"`
}

type codexText struct {
	Text string `json:"text"`
}

// parseCodexTitle reads the opening prompt out of a rollout, but only once
// the rollout says it belongs to this sandbox's workspace. A codex session
// started somewhere else in the container is not this one.
func parseCodexTitle(out []byte, workspace string) string {
	matched := workspace == ""
	for _, line := range jsonLines(out) {
		var rec codexRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type == "session_meta" {
			if workspace != "" && rec.Payload.Cwd != workspace {
				return ""
			}
			matched = true
			continue
		}
		// Until the rollout has identified itself there is nothing tying it
		// to this sandbox, so nothing in it can be shown.
		if !matched {
			continue
		}
		if prompt := codexPrompt(rec); prompt != "" {
			return prompt
		}
	}
	return ""
}

// codexPrompt returns the prompt a record holds, if it holds one a person
// typed. The model's first "user" message is an environment briefing codex
// wrote itself, and there are more such blocks; none of them name the work.
func codexPrompt(rec codexRecord) string {
	var text string
	switch p := rec.Payload; {
	case p.Type == "user_message":
		text = p.Message
	case p.Type == "item_completed" && p.Item.Type == "UserMessage":
		text = firstText(p.Item.Content)
	case p.Type == "message" && p.Role == "user":
		text = firstText(p.Content)
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<") {
		return ""
	}
	return text
}

func firstText(content []codexText) string {
	for _, c := range content {
		if strings.TrimSpace(c.Text) != "" {
			return c.Text
		}
	}
	return ""
}

// --- opencode ---
//
// opencode generates a title too, but keeps its sessions in a database rather
// than in files, so its own CLI is what reads them back. It is in the sandbox
// by definition: it is the agent the sandbox was built for.

func opencodeScript(string) string {
	return `opencode session list --format json -n 20 2>/dev/null`
}

// parseOpencodeTitle takes the most recently touched session in this
// sandbox's workspace. A session that has not been titled yet carries a
// placeholder, which is worth less than the name the manager gave it.
func parseOpencodeTitle(out []byte, workspace string) string {
	start := bytes.IndexByte(out, '[')
	if start < 0 {
		return ""
	}
	var sessions []struct {
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Updated   int64  `json:"updated"`
	}
	if err := json.Unmarshal(out[start:], &sessions); err != nil {
		return ""
	}
	var title string
	var newest int64
	for _, s := range sessions {
		if workspace != "" && s.Directory != workspace {
			continue
		}
		if strings.HasPrefix(s.Title, "New session - ") {
			continue
		}
		if s.Title != "" && s.Updated >= newest {
			title, newest = s.Title, s.Updated
		}
	}
	return title
}

// --- watching ---

// watchTitle follows a session's own title for as long as its process lives.
// It is best-effort throughout: a sandbox that cannot be reached, an agent
// that never writes a title, and a session that ends first all leave the
// session with the name the manager gave it.
func (m *Manager) watchTitle(s *Session, sb *Sandbox) {
	if s.Kind != KindAgent {
		return
	}
	reader, ok := titleReaders[sb.Agent]
	if !ok {
		return
	}
	convID := s.ConvID()
	if reader.pin != nil && convID == "" {
		// This agent can be pinned but this session was not — it continues a
		// conversation of the caller's choosing. Guessing which is worse than
		// showing nothing.
		return
	}

	done := s.Done()
	go func() {
		t := time.NewTicker(titlePoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			// An unpinned agent is matched to its record by position, which
			// stops being true the moment a second agent session shares the
			// sandbox. Whatever was read before then stands; nothing new is
			// attributed to a session that might not own it.
			if reader.pin == nil && !m.soleAgentSession(s) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
			title, err := m.readTitle(ctx, reader, sb, convID)
			cancel()
			if err == nil && title != "" {
				s.setAITitle(title)
			}
		}
	}()
}

// soleAgentSession reports whether s is the only agent session still live in
// its sandbox.
func (m *Manager) soleAgentSession(s *Session) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, other := range m.sessions {
		if other.SandboxID != s.SandboxID || other.ID == s.ID {
			continue
		}
		if other.Kind == KindAgent && other.IsLive() {
			return false
		}
	}
	return true
}

// readTitle runs one poll: the agent's script inside the sandbox, and its
// parser over what came back.
func (m *Manager) readTitle(ctx context.Context, reader titleReader, sb *Sandbox, convID string) (string, error) {
	if convID != "" && !convIDRE.MatchString(convID) {
		return "", fmt.Errorf("invalid conversation id %q", convID)
	}
	out, err := m.client.Exec(ctx, sb.Name, "sh", "-c", reader.script(convID))
	if err != nil {
		return "", err
	}
	return summarize(reader.parse(out, sb.Workspace)), nil
}

// jsonLines splits command output into the lines that could be JSON records.
// sbx writes its own lines to that stream — "Sandbox … started successfully"
// when the exec had to start the container — so records are looked for rather
// than assumed to be the whole output.
func jsonLines(out []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("{")) {
			lines = append(lines, line)
		}
	}
	return lines
}

// summarize reduces whatever an agent handed back to one line a sidebar can
// show. An agent that stores a prompt rather than a summary can hand back
// something arbitrarily long.
func summarize(title string) string {
	for _, line := range strings.Split(title, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return truncate(line, maxAgentTitle)
		}
	}
	return ""
}

// newUUID returns a random RFC 4122 version 4 UUID, the only form claude
// accepts for a conversation ID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failing means the process cannot continue safely
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
