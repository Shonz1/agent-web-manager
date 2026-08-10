package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
// other setting the file already holds. The merged file is handed to the
// shell as an argument rather than built inside it, so nothing in it is read
// as shell syntax.
func (m *Manager) writeModel(ctx context.Context, target string, model json.RawMessage) error {
	host := sandboxHost{client: m.client, name: target}
	settings, err := readSettings(ctx, host, settingsPath)
	if err != nil {
		// Unreadable rather than absent: something is there that this does
		// not understand, and overwriting it would lose whatever it is.
		return err
	}
	settings["model"] = model

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	_, err = m.client.Exec(ctx, target, "sh", "-c",
		fmt.Sprintf(`mkdir -p "$(dirname %q)" && printf '%%s\n' "$1" > %q`, settingsPath, settingsPath),
		"sh", string(data))
	return err
}
