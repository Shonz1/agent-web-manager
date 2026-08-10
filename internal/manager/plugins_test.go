package manager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// gitMarket and repoMarket are the two source shapes Claude Code records.
func gitMarket(url string) knownMarketplace {
	var mk knownMarketplace
	mk.Source.Source, mk.Source.URL = "git", url
	return mk
}

func repoMarket(repo string) knownMarketplace {
	var mk knownMarketplace
	mk.Source.Source, mk.Source.Repo = "github", repo
	return mk
}

func pathMarket(path string) knownMarketplace {
	var mk knownMarketplace
	mk.Source.Source, mk.Source.Path = "local", path
	return mk
}

func TestPlanMirror(t *testing.T) {
	const omcURL = "https://github.com/Yeachan-Heo/oh-my-claudecode.git"

	tests := []struct {
		name        string
		set         pluginSet
		wantMarkets []marketplaceAdd
		wantPlugins []string
		wantSkipped int
	}{
		{
			name: "a plugin brings the marketplace it came from",
			set: pluginSet{
				plugins:      []installedPlugin{{ID: "oh-my-claudecode@omc", Scope: "user", Enabled: true}},
				marketplaces: map[string]knownMarketplace{"omc": gitMarket(omcURL)},
			},
			wantMarkets: []marketplaceAdd{{name: "omc", spec: omcURL}},
			wantPlugins: []string{"oh-my-claudecode@omc"},
		},
		{
			name: "a github marketplace is named as owner/repo",
			set: pluginSet{
				plugins: []installedPlugin{{ID: "github@claude-plugins-official", Enabled: true}},
				marketplaces: map[string]knownMarketplace{
					"claude-plugins-official": repoMarket("anthropics/claude-plugins-official"),
				},
			},
			wantMarkets: []marketplaceAdd{{name: "claude-plugins-official", spec: "anthropics/claude-plugins-official"}},
			wantPlugins: []string{"github@claude-plugins-official"},
		},
		{
			name: "one marketplace is added once however many plugins came from it",
			set: pluginSet{
				plugins: []installedPlugin{
					{ID: "b@omc", Enabled: true},
					{ID: "a@omc", Enabled: true},
				},
				marketplaces: map[string]knownMarketplace{"omc": gitMarket(omcURL)},
			},
			wantMarkets: []marketplaceAdd{{name: "omc", spec: omcURL}},
			wantPlugins: []string{"a@omc", "b@omc"},
		},
		{
			// ~/.claude/skills is a mount every sandbox shares, so these are
			// in the new one before anything is copied into it.
			name: "a skills-dir plugin is already there",
			set: pluginSet{
				plugins: []installedPlugin{{ID: "omc-reference@skills-dir", Enabled: true}},
			},
		},
		{
			name: "a disabled plugin is not copied",
			set: pluginSet{
				plugins:      []installedPlugin{{ID: "oh-my-claudecode@omc", Enabled: false}},
				marketplaces: map[string]knownMarketplace{"omc": gitMarket(omcURL)},
			},
		},
		{
			// The directory it names is on the machine it was added on, which
			// is not the sandbox being filled.
			name: "a local marketplace cannot follow the plugin",
			set: pluginSet{
				plugins:      []installedPlugin{{ID: "mine@dev", Enabled: true}},
				marketplaces: map[string]knownMarketplace{"dev": pathMarket("/Users/someone/plugins")},
			},
			wantSkipped: 1,
		},
		{
			name: "a plugin from a marketplace that is not recorded is skipped",
			set: pluginSet{
				plugins: []installedPlugin{{ID: "ghost@nowhere", Enabled: true}},
			},
			wantSkipped: 1,
		},
		{
			name: "a name that is not plugin@marketplace is skipped",
			set: pluginSet{
				plugins: []installedPlugin{{ID: "bare-name", Enabled: true}},
			},
			wantSkipped: 1,
		},
		{
			// It would be read by claude as one of its own flags rather than
			// as the plugin to install.
			name: "a name that begins with a dash is skipped",
			set: pluginSet{
				plugins:      []installedPlugin{{ID: "--help@omc", Enabled: true}},
				marketplaces: map[string]knownMarketplace{"omc": gitMarket(omcURL)},
			},
			wantSkipped: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, skipped := planMirror(tt.set)

			if !reflect.DeepEqual(plan.marketplaces, tt.wantMarkets) {
				t.Errorf("marketplaces = %+v, want %+v", plan.marketplaces, tt.wantMarkets)
			}
			var got []string
			for _, p := range plan.plugins {
				got = append(got, p.ID)
			}
			if !reflect.DeepEqual(got, tt.wantPlugins) {
				t.Errorf("plugins = %v, want %v", got, tt.wantPlugins)
			}
			if len(skipped) != tt.wantSkipped {
				t.Errorf("skipped %d (%v), want %d", len(skipped), skipped, tt.wantSkipped)
			}
		})
	}
}

// fakeHost answers the two reads a mirror makes.
type fakeHost struct {
	list    string
	markets string
	listErr error
	calls   [][]string
}

func (h *fakeHost) run(_ context.Context, argv ...string) ([]byte, error) {
	h.calls = append(h.calls, argv)
	if len(argv) > 1 && argv[1] == "plugin" {
		if h.listErr != nil {
			return nil, h.listErr
		}
		return []byte(h.list), nil
	}
	return []byte(h.markets), nil
}

func (h *fakeHost) String() string { return "fake" }

func TestReadPluginSet(t *testing.T) {
	h := &fakeHost{
		// sbx prefixes output of its own when the exec had to start the
		// container, and it has to be stepped over rather than parsed.
		list: "Sandbox claude-app started successfully\n" + `[
		  {"id":"oh-my-claudecode@omc","version":"4.15.7","scope":"user","enabled":true},
		  {"id":"github@claude-plugins-official","scope":"user","enabled":false}
		]`,
		markets: `{"omc":{"source":{"source":"git","url":"https://example.com/omc.git"}}}`,
	}

	set, err := readPluginSet(context.Background(), h)
	if err != nil {
		t.Fatalf("readPluginSet: %v", err)
	}
	if len(set.plugins) != 2 {
		t.Fatalf("plugins = %+v, want 2", set.plugins)
	}
	if set.plugins[0].ID != "oh-my-claudecode@omc" || !set.plugins[0].Enabled {
		t.Errorf("first plugin = %+v", set.plugins[0])
	}
	if set.plugins[1].Enabled {
		t.Errorf("second plugin should be disabled: %+v", set.plugins[1])
	}
	if got := set.marketplaces["omc"].Source.URL; got != "https://example.com/omc.git" {
		t.Errorf("marketplace url = %q", got)
	}
}

// A host that has never added a marketplace has no file to read, and "cat"
// says so on stderr. That is a host with nothing to copy, not a broken read.
func TestReadPluginSetWithoutMarketplacesFile(t *testing.T) {
	h := &fakeHost{list: `[]`, markets: ""}
	set, err := readPluginSet(context.Background(), h)
	if err != nil {
		t.Fatalf("readPluginSet: %v", err)
	}
	if len(set.marketplaces) != 0 {
		t.Errorf("marketplaces = %+v, want none", set.marketplaces)
	}
}

// A host with no claude in it cannot be asked, and the caller has to hear
// about it rather than treat it as a host with no plugins.
func TestReadPluginSetPropagatesFailure(t *testing.T) {
	h := &fakeHost{listErr: errors.New("claude: not found")}
	if _, err := readPluginSet(context.Background(), h); err == nil {
		t.Fatal("want an error when the plugin list cannot be read")
	}
}

func TestConfigSource(t *testing.T) {
	m := &Manager{}

	tests := []struct {
		name string
		req  CreateSandboxRequest
		want string // the host's String(), or "" for none
	}{
		{
			name: "a plain claude sandbox takes them from this machine",
			req:  CreateSandboxRequest{Agent: "claude"},
			want: "this machine",
		},
		{
			name: "a session sandbox takes them from the base sandbox it was cloned from",
			req:  CreateSandboxRequest{Agent: "claude", PluginsFrom: "claude-app"},
			want: "sandbox claude-app",
		},
		{
			// Opting out of the plugins says nothing about the model, and this
			// is the source of both — so it still answers. Whether the plugins
			// are actually copied is CreateSandbox's question, tested in
			// TestNoPluginsStillCarriesTheModel.
			name: "opting out of the plugins still leaves a source for the model",
			req:  CreateSandboxRequest{Agent: "claude", NoPlugins: true},
			want: "this machine",
		},
		{
			// Another agent has no claude in it to install them with.
			name: "a codex sandbox has no plugins to speak of",
			req:  CreateSandboxRequest{Agent: "codex"},
		},
		{
			name: "a source that is not a sandbox name is refused",
			req:  CreateSandboxRequest{Agent: "claude", PluginsFrom: "; rm -rf /"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, ok := m.configSource(tt.req)
			if tt.want == "" {
				if ok {
					t.Fatalf("want no source, got %s", from)
				}
				return
			}
			if !ok {
				t.Fatal("want a source, got none")
			}
			if got := from.String(); got != tt.want {
				t.Errorf("source = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJSONFrom(t *testing.T) {
	if got := string(jsonFrom([]byte("noise\n[1,2]"), '[')); got != "[1,2]" {
		t.Errorf("jsonFrom = %q", got)
	}
	if got := jsonFrom([]byte("nothing here"), '{'); got != nil {
		t.Errorf("jsonFrom = %q, want nil", got)
	}
}

// The mirror is best-effort: a source it cannot read leaves the sandbox as it
// was rather than failing the create that made it.
func TestMirrorPluginsSurvivesAnUnreadableSource(t *testing.T) {
	m := &Manager{}
	h := &fakeHost{listErr: errors.New("no claude here")}
	m.mirrorPlugins("claude-app", h)

	if len(h.calls) != 1 || !strings.Contains(strings.Join(h.calls[0], " "), "plugin list") {
		t.Errorf("calls = %v, want a single plugin list", h.calls)
	}
}

// recordingSbx writes a stand-in for sbx that runs what "exec" is given, with a
// HOME of the named sandbox's own, and appends every invocation to a log. The
// separate homes are what let a copy between two sandboxes be told apart from
// one that never happened, and the log is what says which commands were run to
// do it.
func recordingSbx(t *testing.T, root, logPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ls\" ]; then printf '%s' '{\"sandboxes\":[]}'; exit 0; fi\n" +
		"if [ \"$1\" = \"exec\" ]; then\n" +
		"  box=$2; shift 2\n" +
		"  echo \"$box: $*\" >> " + logPath + "\n" +
		"  HOME=" + root + "/$box; export HOME; mkdir -p \"$HOME\"\n" +
		"  \"$@\"; exit $?\n" +
		"fi\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// ranPluginCommand reports whether anything in the log was a "claude plugin"
// call, which is the only way a plugin gets into a sandbox.
func ranPluginCommand(t *testing.T, logPath string) bool {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), "claude plugin")
}

// Turning the plugin copy off must not quietly take the model with it. They are
// two settings on one page, and before this they shared a single decision — so
// a project that wanted no plugins silently stopped passing on the model it had
// been asked to use.
func TestNoPluginsStillCarriesTheModel(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "sbx.log")

	// What the source sandbox is set to, in the file a sandbox keeps it in.
	source := filepath.Join(root, "base-sb", ".claude")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(sbx.New(recordingSbx(t, root, logPath)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.CreateSandbox(CreateSandboxRequest{
		Name:        "target-sb",
		Agent:       "claude",
		Workspace:   t.TempDir(),
		PluginsFrom: "base-sb",
		NoPlugins:   true,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	if ranPluginCommand(t, logPath) {
		t.Error("plugins were copied into a sandbox that asked for none")
	}
	got := settingValue(t, filepath.Join(root, "target-sb", ".claude", "settings.json"), "model")
	if got != `"opus"` {
		t.Errorf("model = %s, want \"opus\" carried across even with the plugins turned off", got)
	}
}

// The setting is the project's, and what it governs is the sandboxes the
// project makes afterwards.
func TestProjectPluginChoiceReachesTheSandboxesItMakes(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "sbx.log")

	m, err := New(sbx.New(recordingSbx(t, root, logPath)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := projectWithBase(t, m, "base-sb")
	if _, err := m.SetProjectPlugins(p.ID, true); err != nil {
		t.Fatalf("SetProjectPlugins: %v", err)
	}

	if _, err := m.CreateSessionSandbox(context.Background(), p.ID, true, nil); err != nil {
		t.Fatalf("CreateSessionSandbox: %v", err)
	}
	if ranPluginCommand(t, logPath) {
		t.Error("a session sandbox was given plugins by a project that had turned the copy off")
	}

	// And back on again: the next sandbox is filled, the ones already made are
	// left as they are.
	if _, err := m.SetProjectPlugins(p.ID, false); err != nil {
		t.Fatalf("SetProjectPlugins back on: %v", err)
	}
	if _, err := m.CreateSessionSandbox(context.Background(), p.ID, true, nil); err != nil {
		t.Fatalf("second CreateSessionSandbox: %v", err)
	}
	if !ranPluginCommand(t, logPath) {
		t.Error("the copy was turned back on and the next sandbox still got none")
	}
}

// The choice outlives the process: it is written with the project rather than
// held in memory for as long as the manager happens to run.
func TestSetProjectPluginsPersists(t *testing.T) {
	stateDir := t.TempDir()
	m, err := New(sbx.New(fakeSbx(t)), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.CreateProject(CreateProjectRequest{Name: "demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if p.NoPlugins {
		t.Error("a new project copies plugins: that is what it did before there was a setting")
	}

	updated, err := m.SetProjectPlugins(p.ID, true)
	if err != nil {
		t.Fatalf("SetProjectPlugins: %v", err)
	}
	if !updated.NoPlugins {
		t.Error("the project still says it copies plugins")
	}

	reloaded, err := New(sbx.New(fakeSbx(t)), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	back, err := reloaded.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !back.NoPlugins {
		t.Error("the choice did not survive a restart")
	}

	if _, err := m.SetProjectPlugins("nope", true); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown project: %v, want ErrProjectNotFound", err)
	}
}
