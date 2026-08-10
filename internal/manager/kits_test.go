package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/sbx"
)

// kitsFolder writes a kits directory holding the named kits, each a directory
// with a manifest in it, and returns it.
func kitsFolder(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		spec := "schemaVersion: \"2\"\nkind: mixin\nname: " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, name, "spec.yaml"), []byte(spec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loggingSbx writes a stand-in for the sbx binary that appends every
// invocation to a log and reports no sandboxes, so a test can read back what
// the manager asked for.
func loggingSbx(t *testing.T, logPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"ls\" ]; then printf '%s' '{\"sandboxes\":[]}'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func sbxLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// A kit is named by name and applied by path: the manager is what turns one
// into the other, against the kits actually installed on this machine.
func TestCreateSandboxAppliesKits(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	logPath := filepath.Join(t.TempDir(), "sbx.log")
	kits := kitsFolder(t, "vale", "kernel")
	stateDir := t.TempDir()

	m, err := New(sbx.New(loggingSbx(t, logPath)), stateDir, WithKitsDir(kits))
	if err != nil {
		t.Fatal(err)
	}

	sb, err := m.CreateSandbox(CreateSandboxRequest{
		Name:      "box",
		Agent:     "shell",
		Workspace: t.TempDir(),
		Kits:      []string{"vale", "kernel", "vale"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	want := "--kit " + filepath.Join(kits, "vale") + " --kit " + filepath.Join(kits, "kernel")
	if got := sbxLog(t, logPath); !strings.Contains(got, want) {
		t.Errorf("sbx was called as %q, want a create carrying %q", got, want)
	}
	// The record keeps the names, once each: they are what the UI shows and
	// what a rebuild resolves again.
	if !slices.Equal(sb.Kits, []string{"vale", "kernel"}) {
		t.Errorf("kits on the record = %q", sb.Kits)
	}

	// And they outlive the process, since a rebuild happens in some later one.
	again, err := New(sbx.New(""), stateDir, WithKitsDir(kits))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := again.GetSandbox(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reloaded.Kits, []string{"vale", "kernel"}) {
		t.Errorf("kits after reload = %q", reloaded.Kits)
	}
}

// The browser names a kit; it does not name a path. Anything the store cannot
// see is refused before a sandbox is made, rather than handed to "--kit".
func TestCreateSandboxRefusesAKitItCannotSee(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	logPath := filepath.Join(t.TempDir(), "sbx.log")
	m, err := New(sbx.New(loggingSbx(t, logPath)), t.TempDir(), WithKitsDir(kitsFolder(t, "vale")))
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.CreateSandbox(CreateSandboxRequest{
		Name:      "box",
		Agent:     "shell",
		Workspace: t.TempDir(),
		Kits:      []string{"vale", "/etc"},
	})
	if err == nil {
		t.Fatal("a sandbox was made with a kit that is not installed")
	}
	if !strings.Contains(err.Error(), "/etc") {
		t.Errorf("error = %v, and should name the kit", err)
	}
	if strings.Contains(sbxLog(t, logPath), "create") {
		t.Error("sbx create ran for a request that was going to be refused")
	}
	if m.ManagedNames()["box"] {
		t.Error("the refused sandbox was registered anyway")
	}
}

// A kit can only go on as a sandbox is made, so a sandbox sbx has lost and
// this rebuilds must be given its kits again — or it comes back under the same
// name without the network policy and tools it was asked for.
func TestEnsureSandboxRebuildsWithItsKits(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	logPath := filepath.Join(t.TempDir(), "sbx.log")
	kits := kitsFolder(t, "vale")
	m, err := New(sbx.New(loggingSbx(t, logPath)), t.TempDir(), WithKitsDir(kits))
	if err != nil {
		t.Fatal(err)
	}

	// The stand-in reports no sandboxes, so this one reads as gone.
	sb := &Sandbox{ID: "id1", Name: "box", Agent: "shell", Workspace: "/w", Kits: []string{"vale"}}
	if err := m.ensureSandbox(context.Background(), sb); err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	want := "--kit " + filepath.Join(kits, "vale")
	if got := sbxLog(t, logPath); !strings.Contains(got, want) {
		t.Errorf("rebuild was %q, want it to carry %q", got, want)
	}

	// A kit that has since been uninstalled stops the rebuild: the sandbox it
	// would produce is not the sandbox that was lost.
	if err := os.RemoveAll(filepath.Join(kits, "vale")); err != nil {
		t.Fatal(err)
	}
	err = m.ensureSandbox(context.Background(), sb)
	if err == nil {
		t.Fatal("rebuilt a sandbox without the kit it was built with")
	}
	if !strings.Contains(err.Error(), "vale") {
		t.Errorf("error = %v, and should name the kit that is missing", err)
	}
}

// A session's kits belong to the session: they are chosen when it is started,
// and they reach the sandbox that is made for it.
func TestSessionSandboxCarriesTheKitsItWasStartedWith(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	logPath := filepath.Join(t.TempDir(), "sbx.log")
	kits := kitsFolder(t, "vale")
	m, err := New(sbx.New(loggingSbx(t, logPath)), t.TempDir(), WithKitsDir(kits))
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.CreateProject(CreateProjectRequest{Name: "Demo", Path: t.TempDir(), Agent: "shell"})
	if err != nil {
		t.Fatal(err)
	}

	sb, err := m.CreateSessionSandbox(context.Background(), p.ID, true, []string{"vale"})
	if err != nil {
		t.Fatalf("CreateSessionSandbox: %v", err)
	}
	if !slices.Equal(sb.Kits, []string{"vale"}) {
		t.Errorf("kits = %q, want the session's own", sb.Kits)
	}
	// The base sandbox is made first and is only ever cloned from, so it takes
	// none of them: a kit belongs to the session that asked for it.
	base := m.BaseSandbox(p.ID)
	if base == nil {
		t.Fatal("the project has no base sandbox")
	}
	if len(base.Kits) != 0 {
		t.Errorf("the base sandbox took the session's kits: %q", base.Kits)
	}
}

// Kits are read when they are asked for, not when the manager starts: a kit
// dropped into the folder while this is running is one the next session can
// be given.
func TestKitsAreReadWhenAsked(t *testing.T) {
	dir := t.TempDir()
	m, err := New(sbx.New(""), t.TempDir(), WithKitsDir(dir))
	if err != nil {
		t.Fatal(err)
	}

	kits, reported, err := m.Kits()
	if err != nil {
		t.Fatalf("Kits: %v", err)
	}
	if len(kits) != 0 || reported != dir {
		t.Fatalf("kits = %+v, dir = %q, want none and %q", kits, reported, dir)
	}

	if err := os.MkdirAll(filepath.Join(dir, "vale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vale", "spec.yaml"), []byte("kind: mixin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kits, _, err = m.Kits()
	if err != nil {
		t.Fatalf("Kits: %v", err)
	}
	if len(kits) != 1 || kits[0].Name != "vale" {
		t.Fatalf("kits = %+v, want the one just installed", kits)
	}
}
