package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// volumeFileSizeCap caps the bytes returned by /file. Anything larger is
// truncated with a marker — UIs are expected to render text snippets,
// not full binaries.
const volumeFileSizeCap = 1 << 20 // 1 MiB

// handleVolumes responds to GET /api/volumes — list all named volumes.
func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	vols, err := s.volumes.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(vols))
	for _, v := range vols {
		out = append(out, map[string]any{
			"name":       v.Name,
			"path":       v.Path,
			"created_at": v.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVolumeScoped routes:
//
//	/api/volumes/{name}/tree       GET file tree
//	/api/volumes/{name}/file?p=... GET file content
func (s *Server) handleVolumeScoped(w http.ResponseWriter, r *http.Request) {
	tail, ok := stripPrefix(r.URL.Path, "/api/volumes/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	name, op := parts[0], parts[1]
	if op == "tree" {
		s.serveVolumeTree(w, r, name)
		return
	}
	if op == "file" {
		s.serveVolumeFile(w, r, name)
		return
	}
	http.NotFound(w, r)
}

// fileTreeEntry is one node in the JSON tree response.
type fileTreeEntry struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`        // relative to volume root, posix-style
	IsDir    bool            `json:"is_dir"`
	Size     int64           `json:"size,omitempty"`
	Children []fileTreeEntry `json:"children,omitempty"`
}

func (s *Server) serveVolumeTree(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	vol, err := s.volumes.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tree, err := buildFileTree(vol.Path, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

func (s *Server) serveVolumeFile(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := r.URL.Query().Get("p")
	if rel == "" {
		http.Error(w, "?p= is required", http.StatusBadRequest)
		return
	}
	vol, err := s.volumes.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Path-traversal defence: reject any rel that escapes the volume root.
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, string(os.PathSeparator)+"..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	abs := filepath.Join(vol.Path, clean)
	if !strings.HasPrefix(abs, vol.Path+string(os.PathSeparator)) && abs != vol.Path {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "is a directory", http.StatusBadRequest)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-File-Size", itoa(info.Size()))
	if info.Size() > volumeFileSizeCap {
		w.Header().Set("X-Truncated", "true")
		// Read up to cap, append a marker so it's obvious the rest was clipped.
		buf := make([]byte, volumeFileSizeCap)
		n, _ := f.Read(buf)
		_, _ = w.Write(buf[:n])
		_, _ = w.Write([]byte("\n--- [truncated; file is larger than 1MB] ---"))
		return
	}
	// Fits in cap — copy.
	_, _ = copyFile(w, f)
}

// buildFileTree walks dir and returns the rooted tree. Hidden files
// (starting with ".") are skipped — keeps the volume browser focused on
// user content. Memory-only KV files under /memory/ are included since
// they are user content too.
func buildFileTree(root, rel string) ([]fileTreeEntry, error) {
	entries, err := os.ReadDir(filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	out := make([]fileTreeEntry, 0, len(entries))
	for _, e := range entries {
		nm := e.Name()
		if strings.HasPrefix(nm, ".") {
			continue
		}
		childRel := filepath.Join(rel, nm)
		entry := fileTreeEntry{
			Name:  nm,
			Path:  filepath.ToSlash(childRel),
			IsDir: e.IsDir(),
		}
		if e.IsDir() {
			children, err := buildFileTree(root, childRel)
			if err == nil {
				entry.Children = children
			}
		} else {
			info, err := e.Info()
			if err == nil {
				entry.Size = info.Size()
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		// Dirs first, then alphabetical.
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// copyFile streams f to w. Defined here (and not just io.Copy inline)
// to keep imports tidy and call sites symmetric.
func copyFile(w http.ResponseWriter, f *os.File) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			nw, werr := w.Write(buf[:n])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
		}
		if err != nil {
			break
		}
	}
	return total, nil
}

// itoa avoids strconv import in the hot path of /file.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
