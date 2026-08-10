package manager

import (
	"context"
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

// A settings file that cannot be parsed is not a host with no model: copying
// nothing would be a silent answer to something that went wrong.
func TestReadModelRejectsUnreadableSettings(t *testing.T) {
	h := settingsHost{files: map[string]string{".claude/settings.json": `{"model":`}}
	if _, err := readModel(context.Background(), h); err == nil {
		t.Fatal("want an error for settings that are not JSON")
	}
}
