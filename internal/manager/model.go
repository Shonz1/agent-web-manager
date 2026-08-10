package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// A sandbox comes up on whatever model its image defaults to, and the model
// someone picked — on this machine, or in a project's base sandbox — is kept
// in a file inside that machine's home directory rather than anywhere sbx
// carries between sandboxes. So a session started in a fresh sandbox would
// quietly be talking to a different model than the last one, which is the
// kind of difference that is only noticed after the work is done.
//
// This copies the choice across the same way plugins.go copies plugins: from
// the base sandbox for a session sandbox, and from this machine for a base
// sandbox. Like that one it is best-effort — a sandbox with the wrong model
// is worth a line in the log, not a failed create.

const (
	// modelReadTimeout bounds reading the setting out of a source. It is a
	// file read either way, so this is generous for what it does.
	modelReadTimeout = 30 * time.Second
	// modelWriteTimeout bounds writing it into the new sandbox, which may
	// have to be started first.
	modelWriteTimeout = 60 * time.Second
)

// settingsPath is Claude Code's user settings file, the documented home of
// the "model" setting and what a sandbox reads on startup.
const settingsPath = "$HOME/.claude/settings.json"

// legacyModelPath is where "claude config set model" used to put the choice.
// It is read as a fallback so a machine that has only ever set the model that
// way still passes it on.
const legacyModelPath = "$HOME/.claude.json"

// mirrorModel gives a freshly created sandbox the model its source is set to.
//
// It blocks, for the reason mirrorPlugins does: the session started in the
// new sandbox has to come up on the right model, not switch to it later.
func (m *Manager) mirrorModel(target string, from pluginHost) {
	readCtx, cancelRead := context.WithTimeout(context.Background(), modelReadTimeout)
	model, err := readModel(readCtx, from)
	cancelRead()
	if err != nil {
		log.Printf("model: %s does not say which model it uses, so %s keeps its default: %v", from, target, err)
		return
	}
	if len(model) == 0 {
		// Nothing was ever chosen there, so there is nothing to pass on and
		// the default is the right answer.
		return
	}

	writeCtx, cancel := context.WithTimeout(context.Background(), modelWriteTimeout)
	defer cancel()
	if err := m.writeModel(writeCtx, target, model); err != nil {
		log.Printf("model: %s keeps its default: %v", target, err)
		return
	}
	log.Printf("model: %s is set to %s, from %s", target, model, from)
}

// readModel returns the source's model setting as the JSON value it is
// stored as, or nothing at all if no model was ever chosen there. It is not
// unwrapped to a string: Claude Code accepts more than a plain name in that
// field, and passing it along unread is both simpler and more faithful than
// deciding here what shapes are allowed.
func readModel(ctx context.Context, h pluginHost) (json.RawMessage, error) {
	for _, path := range []string{settingsPath, legacyModelPath} {
		settings, err := readSettings(ctx, h, path)
		if err != nil {
			return nil, err
		}
		if model, ok := settings["model"]; ok && len(model) > 0 && string(model) != "null" {
			return model, nil
		}
	}
	return nil, nil
}

// readSettings reads one JSON settings file from a host. A file that is not
// there is an empty set of settings rather than a failure: it is what an
// installation nobody has configured looks like, and it is what every sandbox
// starts out as.
//
// The absence is tested for rather than read through: "cat" of a missing file
// fails, and a failed read is how a file that is there but unreadable has to
// be reported — it is the difference between passing the model on and
// overwriting something this does not understand.
func readSettings(ctx context.Context, h pluginHost, path string) (map[string]json.RawMessage, error) {
	out, err := h.run(ctx, "sh", "-c", fmt.Sprintf("if [ -f %[1]q ]; then cat %[1]q; fi", path))
	if err != nil {
		return nil, err
	}
	data := jsonFrom(out, '{')
	if len(data) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return settings, nil
}

// writeModel sets the model in the target sandbox's settings, keeping every
// other setting the file already holds. An empty model takes the setting out
// again, which is how a sandbox is put back on whatever its image defaults to
// — an explicit choice, and not the same as leaving a stale one behind.
//
// The merged file is handed to the shell as an argument rather than built
// inside it, so nothing in it is read as shell syntax.
func (m *Manager) writeModel(ctx context.Context, target string, model json.RawMessage) error {
	host := sandboxHost{client: m.client, name: target}
	settings, err := readSettings(ctx, host, settingsPath)
	if err != nil {
		// Unreadable rather than absent: something is there that this does
		// not understand, and overwriting it would lose whatever it is.
		return err
	}
	if len(model) == 0 {
		delete(settings, "model")
	} else {
		settings["model"] = model
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	_, err = m.client.Exec(ctx, target, "sh", "-c",
		fmt.Sprintf(`mkdir -p "$(dirname %q)" && printf '%%s\n' "$1" > %q`, settingsPath, settingsPath),
		"sh", string(data))
	return err
}

// --- the model a project's sessions are given ---
//
// The base sandbox is where a project's model is kept, because it is the one
// place a change reaches every session started afterwards: nothing runs in
// there, so what it is set to is what the next clone comes up on. Sandboxes
// that already exist are left as they are — their settings file was written
// when they were made, and the agent inside read it once, at startup.

// ErrNoBaseSandbox is returned when a project's base sandbox has not finished
// being built. It is the only thing a project's model can be read from or
// written to, and the wait is the one a session would wait out anyway.
var ErrNoBaseSandbox = errors.New("this project's base sandbox is still being built: its model can be read once it is there")

// modelNameLimit is a generous bound on a model name — the longest real one
// is a fraction of it. It is here so that a paste into the wrong box is
// refused rather than written into a settings file.
const modelNameLimit = 128

// ProjectModelView is the model a project's next session will come up on, and
// the sandbox that decides it.
type ProjectModelView struct {
	// Model is the value as Claude Code stores it, passed on in the shape it
	// is in rather than unwrapped to a name — the field takes more than one,
	// and this is not the place to decide which are allowed. Absent when
	// nothing was ever chosen there, which means the sandbox's own default.
	Model json.RawMessage `json:"model,omitempty"`
	// Sandbox is the base sandbox the value was read from or written to.
	Sandbox string `json:"sandbox"`
}

// ProjectModel reports which model the project's sessions start on.
//
// Reading it runs a command inside the base sandbox, which starts that
// sandbox if it is stopped — so it is asked for when somebody wants to know,
// rather than gathered with the project list every few seconds.
func (m *Manager) ProjectModel(ctx context.Context, projectID string) (ProjectModelView, error) {
	base, err := m.baseSandboxFor(projectID)
	if err != nil {
		return ProjectModelView{}, err
	}
	model, err := readModel(ctx, sandboxHost{client: m.client, name: base.Name})
	if err != nil {
		return ProjectModelView{}, err
	}
	return ProjectModelView{Model: model, Sandbox: base.Name}, nil
}

// SetProjectModel writes a model into the project's base sandbox, so that
// every session sandbox cloned from it afterwards comes up on that model. An
// empty name takes the setting out again and leaves the sandbox on whatever
// its image defaults to.
//
// What it does not do is reach into the sandboxes already there: an agent
// reads its settings when it starts, so a session that is running would not
// notice, and one started later in the same sandbox would come up on a model
// nobody chose for it. The next session gets the new one.
func (m *Manager) SetProjectModel(ctx context.Context, projectID, model string) (ProjectModelView, error) {
	name := strings.TrimSpace(model)
	if err := validModelName(name); err != nil {
		return ProjectModelView{}, err
	}
	base, err := m.baseSandboxFor(projectID)
	if err != nil {
		return ProjectModelView{}, err
	}

	var value json.RawMessage
	if name != "" {
		// Marshalled rather than quoted here: the name goes into a JSON file
		// which is then handed to a shell, and neither is a place to be
		// approximate about escaping.
		encoded, err := json.Marshal(name)
		if err != nil {
			return ProjectModelView{}, err
		}
		value = encoded
	}
	if err := m.writeModel(ctx, base.Name, value); err != nil {
		return ProjectModelView{}, err
	}
	if name == "" {
		log.Printf("model: %s no longer sets one, so sessions cloned from it keep the default", base.Name)
	} else {
		log.Printf("model: %s is set to %s, and every session cloned from it now starts on that", base.Name, name)
	}
	return ProjectModelView{Model: value, Sandbox: base.Name}, nil
}

// baseSandboxFor returns a project's base sandbox, telling a project that is
// not there apart from one whose base sandbox has not been built yet: the
// first is a mistake, the second is a wait.
func (m *Manager) baseSandboxFor(projectID string) (*Sandbox, error) {
	if _, err := m.GetProject(projectID); err != nil {
		return nil, err
	}
	base := m.BaseSandbox(projectID)
	if base == nil {
		return nil, ErrNoBaseSandbox
	}
	return base, nil
}

// validModelName rejects what cannot be a model name. Which names exist is
// not checked and deliberately not known here — Claude Code takes aliases and
// full model ids, and a list kept in this program would be wrong by the time
// anyone read it. A name it does not recognise is the agent's to complain
// about; what this rules out is text that is not a name at all.
func validModelName(name string) error {
	if len(name) > modelNameLimit {
		return fmt.Errorf("model name is %d characters long, which is longer than any model is named", len(name))
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return errors.New("a model name is a single line of plain text")
		}
	}
	return nil
}
