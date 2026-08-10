package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Shonz1/agent-web-manager/internal/sbx"
)

// settingsHost answers the file reads readModel makes, keyed by which file is
// being asked for. Anything not listed is a file that is not there, which is
// what "cat" of a missing settings file leaves: nothing on stdout.
type settingsHost struct {
	files map[string]string
	err   error
}

func (h settingsHost) run(_ context.Context, argv ...string) ([]byte, error) {
	if h.err != nil {
		return nil, h.err
	}
	cmd := strings.Join(argv, " ")
	for path, body := range h.files {
		if strings.Contains(cmd, path) {
			return []byte(body), nil
		}
	}
	return nil, nil
}

func (settingsHost) String() string { return "fake" }

func TestReadModel(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "the settings file is where the model is kept",
			files: map[string]string{".claude/settings.json": `{"model":"opus","theme":"dark"}`},
			want:  `"opus"`,
		},
		{
			// sbx writes a line of its own when the exec had to start the
			// container, exactly as it does for the plugin reads.
			name: "sbx's own output is stepped over",
			files: map[string]string{
				".claude/settings.json": "Sandbox demo started successfully\n" + `{"model":"sonnet"}`,
			},
			want: `"sonnet"`,
		},
		{
			name:  "a machine that only ever used \"claude config\" still passes it on",
			files: map[string]string{".claude.json": `{"model":"haiku"}`},
			want:  `"haiku"`,
		},
		{
			name: "the settings file wins over the older one",
			files: map[string]string{
				".claude/settings.json": `{"model":"opus"}`,
				".claude.json":          `{"model":"haiku"}`,
			},
			want: `"opus"`,
		},
		{
			name:  "nothing was ever chosen",
			files: map[string]string{".claude/settings.json": `{"theme":"dark"}`},
		},
		{
			name: "no settings at all",
		},
		{
			// An explicit null is Claude Code's way of saying "the default",
			// which is what the new sandbox already has.
			name:  "a null model is not a choice",
			files: map[string]string{".claude/settings.json": `{"model":null}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readModel(context.Background(), settingsHost{files: tt.files})
			if err != nil {
				t.Fatalf("readModel: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("model = %s, want %s", got, tt.want)
			}
		})
	}
}

// A sandbox nobody has configured has no settings file at all, and that is
// the state every new one starts in. The real shell command is run here
// because the thing that made this worth writing down is an exit status, not
// an answer: "cat" of a file that is not there fails, and a read that fails is
// a read that stops the model being written anywhere.
func TestReadSettingsOfAHomeWithNoSettings(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	settings, err := readSettings(context.Background(), localHost{}, settingsPath)
	if err != nil {
		t.Fatalf("readSettings of a missing file: %v", err)
	}
	if len(settings) != 0 {
		t.Errorf("settings = %v, want an empty set", settings)
	}

	// And the file is still read when it is there.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	model, err := readModel(context.Background(), localHost{})
	if err != nil {
		t.Fatalf("readModel: %v", err)
	}
	if string(model) != `"opus"` {
		t.Errorf("model = %s, want \"opus\"", model)
	}
}

// A settings file that cannot be parsed is not a host with no model: copying
// nothing would be a silent answer to something that went wrong.
func TestReadModelRejectsUnreadableSettings(t *testing.T) {
	h := settingsHost{files: map[string]string{".claude/settings.json": `{"model":`}}
	if _, err := readModel(context.Background(), h); err == nil {
		t.Fatal("want an error for settings that are not JSON")
	}
}

// execSbx writes a stand-in for the sbx binary whose "exec" really runs the
// command it is given, in a home directory belonging to the test. Both ends of
// a project's model setting are shell commands run inside a sandbox, and what
// is worth checking is that the file one of them writes is the file the other
// reads — which a fake that only records calls could not say.
func execSbx(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ls\" ]; then printf '%s' '{\"sandboxes\":[]}'; exit 0; fi\n" +
		// "exec <sandbox> <argv…>": drop the two that name what to run in.
		"if [ \"$1\" = \"exec\" ]; then shift 2; HOME=" + home + " \"$@\"; exit $?; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// projectWithBase registers a project and a base sandbox for it by hand, as
// the project tests do: CreateSandbox would shell out to sbx, and what these
// are about is what happens to the settings file afterwards.
func projectWithBase(t *testing.T, m *Manager, base string) *Project {
	t.Helper()
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	sb := &Sandbox{ID: "base1", Name: base, Agent: "claude", ProjectID: p.ID, Workspace: p.Path, IsBase: true}
	m.sandboxes[sb.ID] = sb
	m.byName[sb.Name] = sb.ID
	return p
}

// The whole round trip, in the order somebody using it goes through: read what
// the project is set to, set it, read it back, and take it off again. The
// settings the file already held have to survive all of it — a model is one
// setting in a file the sandbox keeps other things in.
func TestProjectModelRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(sbx.New(execSbx(t, home)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := projectWithBase(t, m, "base-sb")
	ctx := context.Background()

	view, err := m.ProjectModel(ctx, p.ID)
	if err != nil {
		t.Fatalf("ProjectModel: %v", err)
	}
	if len(view.Model) != 0 {
		t.Errorf("model = %s, want nothing chosen yet", view.Model)
	}
	if view.Sandbox != "base-sb" {
		t.Errorf("sandbox = %q, want the project's base sandbox", view.Sandbox)
	}

	if view, err = m.SetProjectModel(ctx, p.ID, "  opus  "); err != nil {
		t.Fatalf("SetProjectModel: %v", err)
	}
	if string(view.Model) != `"opus"` {
		t.Errorf("model = %s, want \"opus\" with the spaces off it", view.Model)
	}

	// Read back the way a sandbox cloned from this one would read it.
	read, err := m.ProjectModel(ctx, p.ID)
	if err != nil {
		t.Fatalf("ProjectModel after setting it: %v", err)
	}
	if string(read.Model) != `"opus"` {
		t.Errorf("model = %s, want \"opus\"", read.Model)
	}
	if theme := settingValue(t, settings, "theme"); theme != `"dark"` {
		t.Errorf("theme = %s, want the setting that was already there to survive", theme)
	}

	// Empty is a choice of its own: the setting comes out, and the sandbox is
	// back on its default rather than on a stale name.
	if view, err = m.SetProjectModel(ctx, p.ID, ""); err != nil {
		t.Fatalf("SetProjectModel to nothing: %v", err)
	}
	if len(view.Model) != 0 {
		t.Errorf("model = %s, want it cleared", view.Model)
	}
	if got := settingValue(t, settings, "model"); got != "" {
		t.Errorf("model in the file = %s, want the key gone", got)
	}
	if theme := settingValue(t, settings, "theme"); theme != `"dark"` {
		t.Errorf("theme = %s, want it to survive the clearing too", theme)
	}
}

// settingValue reads one setting back out of a settings file as the JSON it is
// stored as, or "" when the file does not hold it.
func settingValue(t *testing.T, path, key string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse %s: %v (%s)", path, err, data)
	}
	return string(settings[key])
}

// A project whose base sandbox is still being built is not a project that
// cannot have a model: it is one to ask again in a minute, and the difference
// is what the API turns into a status code.
func TestProjectModelBeforeTheBaseSandboxIsThere(t *testing.T) {
	m, err := New(sbx.New(""), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.ProjectModel(context.Background(), p.ID); !errors.Is(err, ErrNoBaseSandbox) {
		t.Errorf("read: %v, want ErrNoBaseSandbox", err)
	}
	if _, err := m.SetProjectModel(context.Background(), p.ID, "opus"); !errors.Is(err, ErrNoBaseSandbox) {
		t.Errorf("write: %v, want ErrNoBaseSandbox", err)
	}
	if _, err := m.ProjectModel(context.Background(), "nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown project: %v, want ErrProjectNotFound", err)
	}
}

// Which names exist is the agent's business, but text that is not a name at
// all is refused before it reaches a settings file — and refused before the
// base sandbox is even looked for, so a bad name cannot start a sandbox.
func TestSetProjectModelRejectsWhatIsNotAName(t *testing.T) {
	m, err := New(sbx.New(""), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := projectWithBase(t, m, "base-sb")

	for _, name := range []string{"opus\ninstall something", strings.Repeat("o", modelNameLimit+1)} {
		if _, err := m.SetProjectModel(context.Background(), p.ID, name); err == nil {
			t.Errorf("SetProjectModel(%q) was accepted", name)
		}
	}
}
