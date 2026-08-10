package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oleksiiipatov/agent-web-manager/internal/manager"
)

// newProject makes a project on a server whose sbx does not exist, which is
// as far as these tests need to get: what they are about is the answers this
// side gives, not what a sandbox says back.
func newProject(t *testing.T, srv *Server) *manager.Project {
	t.Helper()
	p, err := srv.mgr.CreateProject(manager.CreateProjectRequest{
		Name: "Demo", Path: t.TempDir(), Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func modelRequest(t *testing.T, srv *Server, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/projects/"+id+"/model", strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		srv.handleGetProjectModel(rec, req)
	} else {
		srv.handlePutProjectModel(rec, req)
	}
	return rec
}

// The model can only be read once the base sandbox exists, and a project
// whose one is still building is a wait rather than a mistake — which is a
// 409, not the 404 a project that is not there gets.
func TestProjectModelBeforeTheBaseSandboxIsThere(t *testing.T) {
	srv := projectServer(t)
	p := newProject(t, srv)

	rec := modelRequest(t, srv, http.MethodGet, p.ID, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("get status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	rec = modelRequest(t, srv, http.MethodPut, p.ID, `{"model":"opus"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("put status = %d, want 409 (%s)", rec.Code, rec.Body)
	}

	rec = modelRequest(t, srv, http.MethodGet, "nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown project status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// Setting the model decides what every session started afterwards runs, so a
// page on another site must not be able to set it — the same guard the other
// settings endpoints take, and the reason this one is not a plain POST away.
func TestPutProjectModelRejectsCrossOrigin(t *testing.T) {
	srv := projectServer(t)
	p := newProject(t, srv)

	req := httptest.NewRequest(http.MethodPut, "/api/projects/"+p.ID+"/model",
		strings.NewReader(`{"model":"opus"}`))
	req.SetPathValue("id", p.ID)
	req.Header.Set("Origin", "https://elsewhere.example")
	rec := httptest.NewRecorder()
	srv.handlePutProjectModel(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// A name that is not a name is refused here rather than written into a
// settings file — and refused before anything is run in a sandbox, which is
// why it answers even with no sbx to run it with.
func TestPutProjectModelRejectsWhatIsNotAName(t *testing.T) {
	srv := projectServer(t)
	p := newProject(t, srv)

	raw, err := json.Marshal(map[string]string{"model": "opus\nrm -rf /"})
	if err != nil {
		t.Fatal(err)
	}
	rec := modelRequest(t, srv, http.MethodPut, p.ID, string(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
}
