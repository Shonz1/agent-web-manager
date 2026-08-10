package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
