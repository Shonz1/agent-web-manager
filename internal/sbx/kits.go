package sbx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A kit is sbx's own way of giving a sandbox more than its agent image comes
// with: network policy, credentials, environment, files, setup commands, and
// instructions for the agent, written down in one directory and applied with
// "--kit". It is a create-time thing — sbx refuses "--kit" against a sandbox
// that already exists — which is why the manager offers kits where a session
// is started, since that is where its sandbox is made.
//
// Kits can be named by git or registry reference as well as by path, but the
// ones offered here are the ones already on this machine, under ~/.sbx/kits.
// Nothing is fetched to list them, a name in the list is a directory the user
// put there themselves, and the browser never names a path: it names a kit,
// and the name is resolved against the listing below. That last part is the
// point — a kit can install software and open holes in a sandbox's network
// policy, so which ones exist is a decision made on this machine rather than
// in a request.

// kitsSubdir is where sbx keeps a user's own kits, under their home
// directory.
var kitsSubdir = filepath.Join(".sbx", "kits")

// specFile is the manifest at the root of a kit directory. Its presence is
// what makes a directory a kit rather than something else the user left in
// the kits folder.
const specFile = "spec.yaml"

// specReadLimit bounds how much of a spec is read for the two lines of prose
// the UI shows. A kit's manifest is a page long; anything approaching this is
// not one.
const specReadLimit = 64 << 10

// Kit is one kit found in a kits directory.
type Kit struct {
	// Name is what the kit is called here: the directory's name, or a zip's
	// without its extension. It is what a caller asks for, because it is the
	// one part of a kit that means the same thing to the user, to the manager,
	// and in the record of a sandbox made with it.
	Name string `json:"name"`
	// DisplayName and Description are read out of the kit's spec, and are
	// empty for a kit that does not say — a zip, or a spec that cannot be
	// read. The list is not worth failing over either.
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	// Kind is "mixin" for a kit that adds to an agent, "sandbox" for one that
	// defines an agent of its own.
	Kind string `json:"kind,omitempty"`
	// Ref is what "--kit" takes: the absolute path of the directory or zip.
	// It is not published — a browser has no use for a path it is not allowed
	// to name.
	Ref string `json:"-"`
}

// KitStore is a directory of kits kept on this machine.
type KitStore struct {
	dir string
}

// NewKitStore reads kits from dir, or from ~/.sbx/kits when dir is empty.
func NewKitStore(dir string) *KitStore {
	if dir == "" {
		dir = DefaultKitsDir()
	}
	return &KitStore{dir: dir}
}

// DefaultKitsDir is ~/.sbx/kits, or "" on a machine with no home directory —
// which reads, everywhere it is used, as a machine with no kits.
func DefaultKitsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, kitsSubdir)
}

// Dir is where this store looks. The UI shows it, so that a user with no kits
// is told where to put one rather than left with an empty list.
func (s *KitStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// List returns the kits in the directory, by name.
//
// A directory that is not there is not a failure: it is a machine on which
// nobody has installed a kit, which is most of them, and every caller wants
// the same empty list either way.
func (s *KitStore) List() ([]Kit, error) {
	if s == nil || s.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read kits in %s: %w", s.dir, err)
	}

	kits := make([]Kit, 0, len(entries))
	for _, e := range entries {
		if kit, ok := s.kitAt(e.Name()); ok {
			kits = append(kits, kit)
		}
	}
	sort.Slice(kits, func(i, j int) bool { return kits[i].Name < kits[j].Name })
	return kits, nil
}

// kitAt reads one entry of the kits directory, and reports whether it is a
// kit at all.
func (s *KitStore) kitAt(entry string) (Kit, bool) {
	// Hidden entries are the editor's and the operating system's, not the
	// user's — ".DS_Store" is not a kit and neither is ".git".
	if strings.HasPrefix(entry, ".") {
		return Kit{}, false
	}
	path := filepath.Join(s.dir, entry)
	// Stat rather than the ReadDir entry's own type: a kit kept elsewhere and
	// symlinked in here is still a kit, and to a DirEntry it is neither a
	// directory nor a zip.
	info, err := os.Stat(path)
	if err != nil {
		return Kit{}, false
	}

	if !info.IsDir() {
		if !strings.EqualFold(filepath.Ext(entry), ".zip") {
			return Kit{}, false
		}
		return Kit{Name: strings.TrimSuffix(entry, filepath.Ext(entry)), Ref: path}, true
	}
	// A directory without a manifest is something else the user is keeping
	// here. Offering it would mean an "sbx create" that fails on a kit the
	// manager said it had.
	spec, err := os.Stat(filepath.Join(path, specFile))
	if err != nil || spec.IsDir() {
		return Kit{}, false
	}

	kit := Kit{Name: entry, Ref: path}
	fields := readSpecFields(filepath.Join(path, specFile))
	kit.DisplayName = fields["displayName"]
	kit.Description = fields["description"]
	kit.Kind = fields["kind"]
	return kit, true
}

// Resolve looks up the kits a caller asked for by name, in the order they
// were asked for and without repeats.
//
// A name that is not in the listing is refused rather than passed through: it
// is the only thing standing between "--kit" and any path on this machine,
// and it is also the honest answer when a kit has been deleted since the
// sandbox that used it was made.
func (s *KitStore) Resolve(names []string) ([]Kit, error) {
	if len(names) == 0 {
		return nil, nil
	}
	kits, err := s.List()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Kit, len(kits))
	for _, k := range kits {
		byName[k.Name] = k
	}

	out := make([]Kit, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		kit, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("no kit named %q in %s", name, s.Dir())
		}
		seen[name] = true
		out = append(out, kit)
	}
	return out, nil
}

// KitRefs is what "sbx create" takes for a set of kits.
func KitRefs(kits []Kit) []string {
	if len(kits) == 0 {
		return nil
	}
	refs := make([]string, 0, len(kits))
	for _, k := range kits {
		refs = append(refs, k.Ref)
	}
	return refs
}

// KitNames is what a sandbox's record keeps: the names, which survive a kit
// being moved on disk and mean something to the person reading them.
func KitNames(kits []Kit) []string {
	if len(kits) == 0 {
		return nil
	}
	names := make([]string, 0, len(kits))
	for _, k := range kits {
		names = append(names, k.Name)
	}
	return names
}

// readSpecFields reads the top-level scalars of a kit's spec.
//
// By hand rather than with a YAML parser: three lines of prose for a list is
// all that is wanted from a document this program otherwise has no business
// understanding, and a dependency that can parse the rest of it would still
// leave the question of what to do with a kit it could not. A spec this
// cannot read costs the kit its description, not its place in the list.
func readSpecFields(path string) map[string]string {
	fields := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return fields
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, specReadLimit))
	if err != nil {
		return fields
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		// Only the top level: an indented line belongs to a section this does
		// not read, and would otherwise be mistaken for one of its own.
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "displayName", "description", "kind":
			fields[key] = scalar(value)
		}
	}
	return fields
}

// scalar reads a YAML scalar written on one line: quoted or bare, with a
// trailing comment where the value did not claim it.
func scalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
