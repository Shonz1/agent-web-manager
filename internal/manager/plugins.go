package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// Claude Code keeps its plugins in ~/.claude/plugins, and sbx does not carry
// that directory between sandboxes: it belongs to the container's own
// filesystem, unlike ~/.claude/skills, which is a mount every sandbox shares.
// A plugin installed inside a sandbox therefore lives and dies with it, and a
// worktree session — which is a new sandbox every time, because a sandbox's
// mounts are fixed when it is created — would start out with none of them.
//
// So a new sandbox is given whatever the sandbox it was branched from has at
// that moment, and one branched from nothing gets whatever the machine this
// manager runs on has. Nothing here names a plugin: keeping a list would mean
// editing this program every time the list changed, and the list is already
// written down in the place the user installs plugins.
//
// All of it is best-effort. The sandbox is made and usable either way, and a
// marketplace that cannot be reached is worth a line in the log rather than a
// failed create.

const (
	// pluginReadTimeout bounds reading a plugin set back out of a source.
	// Nothing is fetched to answer it.
	pluginReadTimeout = 60 * time.Second
	// pluginStepTimeout bounds one marketplace add or one install. Both are
	// git clones and some are hundreds of megabytes, so it is generous — but
	// bounded, so one unreachable source cannot hold up the rest.
	pluginStepTimeout = 5 * time.Minute
	// pluginMirrorTimeout bounds the whole copy, however many steps it takes.
	pluginMirrorTimeout = 15 * time.Minute
)

// skillsDirMarketplace is the marketplace Claude Code reports for plugins kept
// in ~/.claude/skills. That directory is a mount shared by every sandbox, so
// those plugins are in the new one already and copying one would install a
// second copy of something that was never missing.
const skillsDirMarketplace = "skills-dir"

// installedPlugin is one entry of "claude plugin list --json".
type installedPlugin struct {
	// ID is "<plugin>@<marketplace>", which is also what "plugin install"
	// takes as its argument.
	ID      string `json:"id"`
	Scope   string `json:"scope"`
	Enabled bool   `json:"enabled"`
}

// knownMarketplace is one entry of ~/.claude/plugins/known_marketplaces.json.
// Which of the source fields is set depends on what kind of source it is.
type knownMarketplace struct {
	Source struct {
		Source string `json:"source"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Path   string `json:"path"`
	} `json:"source"`
}

// pluginSet is what a machine has: the plugins installed on it, and the
// marketplaces they were installed from.
type pluginSet struct {
	plugins      []installedPlugin
	marketplaces map[string]knownMarketplace
}

// pluginHost is somewhere a plugin set can be read from. A sandbox is reached
// with "sbx exec"; the machine this manager runs on is reached directly, and
// is the sensible default because it is where a user who installs a plugin
// normally installs it.
type pluginHost interface {
	run(ctx context.Context, argv ...string) ([]byte, error)
	String() string
}

type sandboxHost struct {
	client *sbx.Client
	name   string
}

func (h sandboxHost) run(ctx context.Context, argv ...string) ([]byte, error) {
	return h.client.Exec(ctx, h.name, argv...)
}

func (h sandboxHost) String() string { return "sandbox " + h.name }

type localHost struct{}

func (localHost) run(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %s", argv[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return out, nil
}

func (localHost) String() string { return "this machine" }

// pluginSource picks where a new sandbox's plugins should come from, or
// reports that it should not be given any.
func (m *Manager) pluginSource(req CreateSandboxRequest) (pluginHost, bool) {
	// Plugins are a Claude Code notion. A sandbox built for another agent has
	// no claude in it to install them with, and no use for them if it did.
	if req.NoPlugins || req.Agent != agentClaude {
		return nil, false
	}
	if req.PluginsFrom == "" {
		return localHost{}, true
	}
	if !sbx.ValidName(req.PluginsFrom) {
		return nil, false
	}
	return sandboxHost{client: m.client, name: req.PluginsFrom}, true
}

// mirrorPlugins gives a freshly created sandbox the plugins that from has.
//
// It blocks, because the point of it is that the session started in the new
// sandbox has the plugins when it starts, not some minutes afterwards. A
// create already carries an image pull's worth of budget for the same reason.
func (m *Manager) mirrorPlugins(target string, from pluginHost) {
	ctx, cancel := context.WithTimeout(context.Background(), pluginMirrorTimeout)
	defer cancel()

	readCtx, cancelRead := context.WithTimeout(ctx, pluginReadTimeout)
	set, err := readPluginSet(readCtx, from)
	cancelRead()
	if err != nil {
		log.Printf("plugins: %s keeps none that can be read, so %s gets none: %v", from, target, err)
		return
	}

	plan, skipped := planMirror(set)
	for _, reason := range skipped {
		log.Printf("plugins: %s: %s", target, reason)
	}
	if len(plan.plugins) == 0 {
		return
	}

	log.Printf("plugins: copying %d from %s into %s", len(plan.plugins), from, target)
	for _, mk := range plan.marketplaces {
		// An add that fails is reported but not fatal: the marketplace may
		// already be there, which is the usual answer for the official one.
		if err := m.pluginStep(ctx, target, "marketplace", "add", mk.spec); err != nil {
			log.Printf("plugins: %s: marketplace %s: %v", target, mk.name, err)
		}
	}
	for _, p := range plan.plugins {
		// User scope, whatever scope it had at the source. Project scope would
		// write into the workspace's own .claude/settings.json, and for a
		// worktree that is a tracked file: the sandbox would come up with the
		// checkout already dirty.
		if err := m.pluginStep(ctx, target, "install", p.ID, "--scope", "user"); err != nil {
			log.Printf("plugins: %s: install %s: %v", target, p.ID, err)
			continue
		}
		log.Printf("plugins: %s: installed %s", target, p.ID)
	}
}

// pluginStep runs one "claude plugin ..." inside the target sandbox.
func (m *Manager) pluginStep(ctx context.Context, target string, args ...string) error {
	stepCtx, cancel := context.WithTimeout(ctx, pluginStepTimeout)
	defer cancel()
	argv := append([]string{"claude", "plugin"}, args...)
	_, err := m.client.Exec(stepCtx, target, argv...)
	return err
}

// readPluginSet asks a host what it has. Both halves are read as they are
// stored rather than reconstructed: the plugin list is what its own CLI
// reports, and the marketplaces are the file Claude Code keeps them in.
func readPluginSet(ctx context.Context, h pluginHost) (pluginSet, error) {
	out, err := h.run(ctx, "claude", "plugin", "list", "--json")
	if err != nil {
		return pluginSet{}, err
	}
	var plugins []installedPlugin
	if err := json.Unmarshal(jsonFrom(out, '['), &plugins); err != nil {
		return pluginSet{}, fmt.Errorf("parse plugin list: %w", err)
	}

	// A host with no marketplaces has no file, which is not a failure — it is
	// a host with nothing to copy, and the plan below will say so.
	markets := map[string]knownMarketplace{}
	out, err = h.run(ctx, "sh", "-c", `cat "$HOME/.claude/plugins/known_marketplaces.json" 2>/dev/null`)
	if err == nil {
		if data := jsonFrom(out, '{'); len(data) > 0 {
			if err := json.Unmarshal(data, &markets); err != nil {
				return pluginSet{}, fmt.Errorf("parse marketplaces: %w", err)
			}
		}
	}
	return pluginSet{plugins: plugins, marketplaces: markets}, nil
}

// marketplaceAdd is a marketplace to register before anything can be installed
// from it.
type marketplaceAdd struct {
	name string
	// spec is what "plugin marketplace add" takes: an "owner/repo", or a URL.
	spec string
}

// mirrorPlan is the work a plugin set implies, in the order it has to happen.
type mirrorPlan struct {
	marketplaces []marketplaceAdd
	plugins      []installedPlugin
}

// planMirror decides what of a plugin set can be copied, and returns alongside
// it a line for each thing that cannot be — a plugin quietly missing from the
// new sandbox is the failure this whole file exists to prevent, so the ones
// that stay missing are said out loud.
func planMirror(set pluginSet) (mirrorPlan, []string) {
	var (
		plan    mirrorPlan
		skipped []string
		needed  = map[string]string{} // marketplace name -> spec
	)

	for _, p := range set.plugins {
		if !p.Enabled {
			continue
		}
		name, market, ok := strings.Cut(p.ID, "@")
		if !ok || name == "" || market == "" {
			skipped = append(skipped, fmt.Sprintf("skipped %q: not a plugin@marketplace name", p.ID))
			continue
		}
		if market == skillsDirMarketplace {
			// Already there: ~/.claude/skills is mounted into every sandbox.
			continue
		}
		// Nothing here reaches a shell — every argument is passed to sbx as
		// its own word — but a name that begins with a dash would still be
		// read by claude as one of its own flags.
		if strings.HasPrefix(p.ID, "-") {
			skipped = append(skipped, fmt.Sprintf("skipped %q: a plugin name cannot begin with a dash", p.ID))
			continue
		}
		spec := marketplaceSpec(set.marketplaces[market])
		if spec == "" {
			skipped = append(skipped, fmt.Sprintf(
				"skipped %q: marketplace %q has no source that means anything in another sandbox", p.ID, market))
			continue
		}
		needed[market] = spec
		plan.plugins = append(plan.plugins, p)
	}

	for name, spec := range needed {
		plan.marketplaces = append(plan.marketplaces, marketplaceAdd{name: name, spec: spec})
	}
	// Map order is not order. Sorting both makes a mirror of the same set the
	// same sequence of commands every time, which is what the tests read and
	// what makes a log of one comparable with the next.
	sort.Slice(plan.marketplaces, func(i, j int) bool { return plan.marketplaces[i].name < plan.marketplaces[j].name })
	sort.Slice(plan.plugins, func(i, j int) bool { return plan.plugins[i].ID < plan.plugins[j].ID })
	return plan, skipped
}

// marketplaceSpec renders a marketplace back into the argument that would add
// it somewhere else, or "" if there is no such argument.
func marketplaceSpec(mk knownMarketplace) string {
	switch {
	case mk.Source.Repo != "":
		return mk.Source.Repo
	case mk.Source.URL != "":
		return mk.Source.URL
	}
	// What is left is a local marketplace: a directory on the machine it was
	// added on, which is not a directory in the sandbox being filled.
	return ""
}

// jsonFrom finds the JSON value in command output. sbx writes lines of its own
// to the same stream — "Sandbox … started successfully" when the exec had to
// start the container first — so the value is looked for rather than assumed
// to be the whole of it.
func jsonFrom(out []byte, open byte) []byte {
	if i := strings.IndexByte(string(out), open); i >= 0 {
		return out[i:]
	}
	return nil
}
