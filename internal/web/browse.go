package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxDirEntries caps a single listing so a directory with tens of thousands of
// children cannot stall the picker.
const maxDirEntries = 1000

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Repo bool   `json:"repo,omitempty"`
}

// handleBrowse lists the sub-directories of a host directory so the create
// dialog can offer a folder picker. Browsers cannot hand a real host path to
// the page, so the server has to do the walking.
//
// Only directory names are returned — never file contents — and the manager
// already mounts arbitrary host directories into sandboxes on request, so this
// exposes nothing the API did not already reach.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
		return
	}

	home, _ := os.UserHomeDir()

	dir, err := resolveBrowsePath(r.URL.Query().Get("path"), home)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	info, err := os.Stat(dir)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !info.IsDir() {
		// Selecting a file in the picker should land on its directory rather
		// than error out.
		dir = filepath.Dir(dir)
	}

	names, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}

	showHidden := r.URL.Query().Get("hidden") == "1"
	entries := make([]dirEntry, 0, len(names))
	truncated := false
	for _, e := range names {
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		if !isDir(e, full) {
			continue
		}
		if len(entries) >= maxDirEntries {
			truncated = true
			break
		}
		entries = append(entries, dirEntry{Name: name, Path: full, Repo: isRepo(full)})
	}

	sort.Slice(entries, func(i, j int) bool {
		li, lj := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if li != lj {
			return li < lj
		}
		return entries[i].Name < entries[j].Name
	})

	resp := map[string]any{
		"path":      dir,
		"home":      home,
		"entries":   entries,
		"truncated": truncated,
	}
	if parent := filepath.Dir(dir); parent != dir {
		resp["parent"] = parent
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveBrowsePath turns a user-supplied path into an absolute one, defaulting
// to the home directory and expanding a leading "~".
func resolveBrowsePath(p, home string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		if home == "" {
			return os.Getwd()
		}
		return home, nil
	}
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return filepath.Abs(p)
}

// isDir treats a symlink pointing at a directory as a directory, which is what
// a user picking a workspace expects.
func isDir(e os.DirEntry, full string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && info.IsDir()
}

// isRepo marks git checkouts so project directories stand out in the picker.
func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}
