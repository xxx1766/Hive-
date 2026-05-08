package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// uploadsSubdir is where browser-uploaded artifacts land inside a Volume.
// Kept distinct from `memory/` (KV scope used by agents via memory/* RPC)
// so user uploads and agent-written keys don't visually collide when an
// agent enumerates the bind-mount.
const uploadsSubdir = "uploads"

// defaultUploadCapBytes is the per-request size cap for both multipart
// uploads and URL fetches. Override via HIVE_UPLOAD_MAX_MB.
const defaultUploadCapBytes = 50 << 20 // 50 MiB

// fetchTimeout caps server-side URL fetches.
const fetchTimeout = 30 * time.Second

// uploadCap returns the configured per-request size cap in bytes. Reads
// HIVE_UPLOAD_MAX_MB lazily so a daemon restart isn't required to bump it.
func uploadCap() int64 {
	if s := os.Getenv("HIVE_UPLOAD_MAX_MB"); s != "" {
		if mb, err := strconv.ParseInt(s, 10, 64); err == nil && mb > 0 {
			return mb << 20
		}
	}
	return defaultUploadCapBytes
}

// uploadVolumeFile handles POST /api/volumes/{name}/files (multipart).
// Expects a single file part named "file"; writes it atomically to
// <vol>/uploads/<basename>. Refuses overwrites (409) so a second upload
// with the same filename surfaces explicitly rather than silently
// clobbering an earlier dataset.
func (s *Server) uploadVolumeFile(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	vol, err := s.volumes.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	maxBytes := uploadCap()
	// Limit the entire request body, not just the multipart parser's
	// in-memory buffer — that's the only way to reject huge bodies before
	// they reach the filesystem.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "invalid multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	f, fh, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "form field 'file' is required", http.StatusBadRequest)
		return
	}
	defer f.Close()

	dst, err := safeUploadPath(vol.Path, fh.Filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(dst); err == nil {
		http.Error(w, "file already exists; rename or DELETE first", http.StatusConflict)
		return
	}
	written, err := writeWithCap(dst, f, maxBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volume": vol.Name,
		"path":   path.Join(uploadsSubdir, filepath.Base(dst)),
		"size":   written,
	})
}

// fetchVolumeFile handles POST /api/volumes/{name}/fetch with body
// {"url": "...", "filename": "..."}. Server-side HTTP GET, capped by the
// same uploadCap, with an SSRF guard rejecting private/loopback ranges.
func (s *Server) fetchVolumeFile(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		URL      string `json:"url"`
		Filename string `json:"filename,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	vol, err := s.volumes.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := validateFetchURL(p.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filename := p.Filename
	if filename == "" {
		filename = filenameFromURL(p.URL)
	}
	dst, err := safeUploadPath(vol.Path, filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(dst); err == nil {
		http.Error(w, "file already exists; rename or DELETE first", http.StatusConflict)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", "hive-fetch/1")
	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "fetch: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("fetch: upstream %s", resp.Status), http.StatusBadGateway)
		return
	}
	maxBytes := uploadCap()
	written, err := writeWithCap(dst, resp.Body, maxBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volume": vol.Name,
		"path":   path.Join(uploadsSubdir, filepath.Base(dst)),
		"size":   written,
	})
}

// serveVolumeFilePut handles PUT /api/volumes/{name}/file?p=<rel>.
// Body bytes overwrite the file at the resolved path. Atomic via temp
// + rename, capped at the upload-max env, parents created lazily.
//
// Used by the SPA's in-browser editor — Save sends the textarea
// content back here. Path-traversal guarded the same way as the GET.
func (s *Server) serveVolumeFilePut(w http.ResponseWriter, r *http.Request, name string) {
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
	// Inline path-traversal guard (mirrors resolveVolumeFile, but we
	// don't require the file to already exist — Save can create new).
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, string(os.PathSeparator)+"..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	abs := filepath.Join(vol.Path, clean)
	if !strings.HasPrefix(abs, vol.Path+string(os.PathSeparator)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		http.Error(w, "mkdir parent: "+err.Error(), http.StatusInternalServerError)
		return
	}
	maxBytes := uploadCap()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	written, err := writeWithCap(abs, r.Body, maxBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volume": vol.Name,
		"path":   filepath.ToSlash(clean),
		"size":   written,
	})
}

// safeUploadPath joins volRoot/uploads/<basename(filename)> with strict
// validation: no path separators, no dotfiles, no `..`, must resolve
// inside volRoot/uploads/.
func safeUploadPath(volRoot, raw string) (string, error) {
	base := filepath.Base(strings.TrimSpace(raw))
	if base == "" || base == "." || base == ".." || base == string(os.PathSeparator) {
		return "", errors.New("filename: empty or invalid")
	}
	if strings.HasPrefix(base, ".") {
		return "", errors.New("filename: dotfiles not allowed")
	}
	if strings.ContainsAny(base, "/\\") {
		return "", errors.New("filename: must be a basename, not a path")
	}
	uploadDir := filepath.Join(volRoot, uploadsSubdir)
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir uploads: %w", err)
	}
	abs := filepath.Join(uploadDir, base)
	clean := filepath.Clean(abs)
	if !strings.HasPrefix(clean, uploadDir+string(os.PathSeparator)) {
		return "", errors.New("filename: resolves outside uploads/")
	}
	return clean, nil
}

// writeWithCap streams src into dst (atomic via tmp+rename), bounded by
// maxBytes. Returns the bytes written. Cleanup on any error.
func writeWithCap(dst string, src io.Reader, maxBytes int64) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	limited := &io.LimitedReader{R: src, N: maxBytes + 1}
	written, err := io.Copy(tmp, limited)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return 0, fmt.Errorf("write: %w", err)
	}
	if written > maxBytes {
		_ = tmp.Close()
		cleanup()
		return 0, fmt.Errorf("payload exceeds %d bytes", maxBytes)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return 0, fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return 0, fmt.Errorf("rename: %w", err)
	}
	return written, nil
}

// validateFetchURL gates the URL fed to /api/volumes/{name}/fetch:
//   - scheme must be http or https
//   - host must resolve to at least one global-unicast address; reject
//     loopback / private / link-local. Also reject literal IP forms in
//     those ranges before resolution to short-circuit obvious abuse.
//
// Anyone reachable to the daemon's HTTP port can otherwise pivot the
// daemon into a confused deputy against internal services. Daemons that
// need to fetch from internal hosts should use the agent-side memory/*
// API instead, which is gated by Rank.
func validateFetchURL(raw string) error {
	if raw == "" {
		return errors.New("url: required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url: scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url: missing host")
	}
	// Literal IP fast path.
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("url: host %s is in a blocked range", host)
		}
		return nil
	}
	// DNS path — resolve and check every returned address. There's a TOCTOU
	// gap between this resolution and the http.Client's own resolution; we
	// accept it. The blast radius of a winning rebind is one fetch, capped
	// in size, written to a path the user already controls.
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("url: resolve %s: %w", host, err)
	}
	for _, ip := range addrs {
		if isBlockedIP(ip) {
			return fmt.Errorf("url: host %s resolves to blocked range %s", host, ip)
		}
	}
	return nil
}

// isBlockedIP returns true for loopback, link-local, multicast, broadcast,
// unspecified, and IETF private ranges (10/8, 172.16/12, 192.168/16, fc00::/7).
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 169 && ip4[1] == 254: // covered by IsLinkLocalUnicast but defensive
			return true
		}
		return false
	}
	// IPv6 ULA fc00::/7
	if ip[0]&0xfe == 0xfc {
		return true
	}
	return false
}

// filenameFromURL extracts the trailing path segment to use as a default
// filename when the caller doesn't supply one. Returns "download" when
// the URL has no useful tail.
func filenameFromURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			return base
		}
	}
	return "download"
}
