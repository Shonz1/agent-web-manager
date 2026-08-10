package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Shonz1/agent-web-manager/internal/git"
	"github.com/Shonz1/agent-web-manager/internal/manager"
	"github.com/Shonz1/agent-web-manager/internal/sbx"
)

// kitsServer returns a server whose kits come from a folder of the test's
// own, backed by an sbx binary that deliberately does not exist: listing kits
// reads a directory and never runs sbx.
func kitsServer(t *testing.T, kitsDir string) *Server {
	t.Helper()
	mgr, err := manager.New(
		sbx.New(filepath.Join(t.TempDir(), "no-such-sbx")),
		t.TempDir(),
		manager.WithKitsDir(kitsDir),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{mgr: mgr, git: git.New("")}
}

type kitsResponse struct {
	Kits  []sbx.Kit `json:"kits"`
	Dir   string    `json:"dir"`
	Error string    `json:"error"`
}

func getKits(t *testing.T, srv *Server) kitsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleKits(rec, httptest.NewRequest(http.MethodGet, "/api/kits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	return decodeJSON[kitsResponse](t, rec)
}

func TestHandleKitsListsWhatIsInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vale"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "kind: mixin\ndisplayName: Vale\ndescription: Prose linting\n"
	if err := os.WriteFile(filepath.Join(dir, "vale", "spec.yaml"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	got := getKits(t, kitsServer(t, dir))
	if len(got.Kits) != 1 {
		t.Fatalf("kits = %+v, want one", got.Kits)
	}
	if got.Kits[0].Name != "vale" || got.Kits[0].DisplayName != "Vale" || got.Kits[0].Description != "Prose linting" {
		t.Errorf("got %+v", got.Kits[0])
	}
	// Where they live, so a user with none is told where one would go.
	if got.Dir != dir {
		t.Errorf("dir = %q, want %q", got.Dir, dir)
	}
}

// A machine with no kits folder is the ordinary case. It answers with an empty
// list rather than an error — and never null, which the UI iterates without a
// guard.
func TestHandleKitsWithoutTheFolder(t *testing.T) {
	got := getKits(t, kitsServer(t, filepath.Join(t.TempDir(), "no-such-dir")))
	if got.Kits == nil {
		t.Fatal("kits came back null")
	}
	if len(got.Kits) != 0 || got.Error != "" {
		t.Fatalf("got %+v", got)
	}
}

// The dialog sends the kit names it was shown, and they have to reach the
// manager: it is what turns them into "--kit" arguments, and what refuses one
// it cannot see. A refusal naming the kit is how that arrives back here.
func TestStartProjectSessionCarriesKitsToTheManager(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	kitsDir := t.TempDir()
	mgr, err := manager.New(sbx.New(stubSbx(t)), t.TempDir(), manager.WithKitsDir(kitsDir))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{mgr: mgr, git: git.New("")}

	proj, err := mgr.CreateProject(manager.CreateProjectRequest{
		Name: "Demo", Path: t.TempDir(), Agent: "shell",
	})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"kind":"agent","kits":["nope"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+proj.ID+"/sessions", strings.NewReader(body))
	req.SetPathValue("id", proj.ID)
	rec := httptest.NewRecorder()
	srv.handleStartProjectSession(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("body = %s, want it to name the kit that is not installed", rec.Body)
	}
}

// stubSbx writes a stand-in for the sbx binary that succeeds at everything and
// reports no sandboxes, so the base sandbox this project needs can be made
// without a real one.
func stubSbx(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	script := "#!/bin/sh\nif [ \"$1\" = \"ls\" ]; then printf '%s' '{\"sandboxes\":[]}'; fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
