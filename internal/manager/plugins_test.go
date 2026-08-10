package manager

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
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
			name: "opting out takes them from nowhere",
			req:  CreateSandboxRequest{Agent: "claude", NoPlugins: true},
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
