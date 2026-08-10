package sbx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// kitsDir writes a kits folder holding one of everything the store has an
// opinion about, and returns it.
func kitsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	writeKit(t, dir, "vale", `schemaVersion: "2"
kind: mixin
name: vale
displayName: Vale
description: Prose linting, and the styles it needs
permissions:
  network:
    # An indented "description" belongs to this section, not to the kit.
    description: not the kit's own
`)
	// A directory with no manifest is something else the user keeps here.
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A packed kit is a zip, and says nothing about itself until sbx opens it.
	if err := os.WriteFile(filepath.Join(dir, "packed.zip"), []byte("PK"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Neither of these is a kit.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeKit(t, dir, ".hidden", "kind: mixin\n")

	return dir
}

func writeKit(t *testing.T, dir, name, spec string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, specFile), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKitStoreList(t *testing.T) {
	store := NewKitStore(kitsDir(t))

	kits, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, k := range kits {
		names = append(names, k.Name)
	}
	// A directory without a manifest, a hidden one, and a plain file are not
	// kits; a zip is, under its name without the extension.
	if !slices.Equal(names, []string{"packed", "vale"}) {
		t.Fatalf("kits = %q, want packed and vale", names)
	}

	vale := kits[1]
	if vale.DisplayName != "Vale" || vale.Kind != "mixin" {
		t.Errorf("got %+v", vale)
	}
	if vale.Description != "Prose linting, and the styles it needs" {
		t.Errorf("description = %q, and an indented one must not be mistaken for it", vale.Description)
	}
	if vale.Ref != filepath.Join(store.Dir(), "vale") {
		t.Errorf("ref = %q, want the kit's own directory", vale.Ref)
	}
	if kits[0].DisplayName != "" {
		t.Errorf("a zip says nothing about itself, got %+v", kits[0])
	}
}

// A machine where nobody has installed a kit is the ordinary case, not a
// failure: every caller wants the same empty list.
func TestKitStoreListWithoutTheFolder(t *testing.T) {
	store := NewKitStore(filepath.Join(t.TempDir(), "no-such-dir"))
	kits, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(kits) != 0 {
		t.Fatalf("kits = %+v, want none", kits)
	}
}

func TestKitStoreResolve(t *testing.T) {
	store := NewKitStore(kitsDir(t))

	// In the order asked for, once each, and blanks dropped.
	kits, err := store.Resolve([]string{"vale", "packed", "vale", "  "})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !slices.Equal(KitNames(kits), []string{"vale", "packed"}) {
		t.Fatalf("names = %q", KitNames(kits))
	}
	want := []string{filepath.Join(store.Dir(), "vale"), filepath.Join(store.Dir(), "packed.zip")}
	if !slices.Equal(KitRefs(kits), want) {
		t.Fatalf("refs = %q, want %q", KitRefs(kits), want)
	}
}

// The listing is the only thing standing between "--kit" and any path on this
// machine, so a name that is not in it is refused rather than passed through.
func TestKitStoreResolveRefusesWhatItCannotSee(t *testing.T) {
	store := NewKitStore(kitsDir(t))

	for _, name := range []string{"nope", "notes", "../../etc", "/etc/passwd", "README.md"} {
		if _, err := store.Resolve([]string{name}); err == nil {
			t.Errorf("Resolve(%q) was allowed", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("Resolve(%q): %v — the error should name the kit", name, err)
		}
	}
}

func TestReadSpecFields(t *testing.T) {
	dir := t.TempDir()
	writeKit(t, dir, "quoted", `kind: sandbox
displayName: "Tools, and # not a comment"
description: 'Single quoted'
`)
	writeKit(t, dir, "bare", `displayName: Bare   # trailing comment
description:
`)

	quoted := readSpecFields(filepath.Join(dir, "quoted", specFile))
	if quoted["displayName"] != "Tools, and # not a comment" {
		t.Errorf("displayName = %q", quoted["displayName"])
	}
	if quoted["description"] != "Single quoted" || quoted["kind"] != "sandbox" {
		t.Errorf("got %+v", quoted)
	}

	bare := readSpecFields(filepath.Join(dir, "bare", specFile))
	if bare["displayName"] != "Bare" || bare["description"] != "" {
		t.Errorf("got %+v", bare)
	}
}
