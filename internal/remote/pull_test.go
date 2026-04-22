package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anne-x/hive/internal/store"
)

// newTestPuller wires a Puller to a test server whose responses are
// supplied via a map of {urlPath: body}. Returns the Puller and a fn
// that must be called to tear down the server.
func newTestPuller(t *testing.T, responses map[string]string) (*Puller, func()) {
	t.Helper()
	mux := http.NewServeMux()
	for path, body := range responses {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
	}
	// Catch-all 404 for paths we didn't wire up — matches GitHub's raw behavior.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	st := store.New(t.TempDir())
	p := &Puller{HTTP: srv.Client(), Store: st}
	return p, srv.Close
}

// rawServerRef builds a Ref whose RawURL() points at the test server.
// This is a little hack: we override the fetcher's base by giving the
// Ref a Host that httptest understands. Since our code calls RawURL()
// which hardcodes raw.githubusercontent.com, we can't redirect easily.
// Instead, substitute the hostname at the Puller level.
//
// Simplest approach: run a test-local Puller variant. But we want to
// test the real Puller.Pull path. So we hijack the HTTP client's
// Transport to swap hosts.
func TestPullAgent_SkillKind(t *testing.T) {
	// Simulate raw.githubusercontent.com/x/y/main/path/agent.yaml + SKILL.md
	manifest := `
name: brief
version: 0.1.0
kind: skill
skill: SKILL.md
model: gpt-4o-mini
tools: [net, fs]
rank: staff
`
	skill := "# brief\nYou summarise."

	mux := http.NewServeMux()
	mux.HandleFunc("/x/y/main/p/agent.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(manifest))
	})
	mux.HandleFunc("/x/y/main/p/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(skill))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Build a Puller whose HTTP client rewrites raw.githubusercontent.com →
	// our test server. This lets Puller.Pull exercise the real URL path.
	rt := &hostRewriter{to: srv.URL, inner: srv.Client().Transport}
	st := store.New(t.TempDir())
	p := &Puller{HTTP: &http.Client{Transport: rt}, Store: st}

	ref := &Ref{Host: "github.com", Owner: "x", Repo: "y", Path: "p", Ref: "main"}
	img, err := p.PullAgent(context.Background(), ref)
	if err != nil {
		t.Fatalf("PullAgent: %v", err)
	}
	if img.Manifest.Name != "brief" || img.Manifest.Kind != "skill" {
		t.Fatalf("unexpected manifest: %+v", img.Manifest)
	}
}

func TestPullAgent_BinaryKindRejected(t *testing.T) {
	manifest := `
name: fetch
version: 0.1.0
kind: binary
entry: bin/fetch
rank: intern
`
	mux := http.NewServeMux()
	mux.HandleFunc("/x/y/main/p/agent.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(manifest))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt := &hostRewriter{to: srv.URL, inner: srv.Client().Transport}
	st := store.New(t.TempDir())
	p := &Puller{HTTP: &http.Client{Transport: rt}, Store: st}

	ref := &Ref{Host: "github.com", Owner: "x", Repo: "y", Path: "p", Ref: "main"}
	_, err := p.PullAgent(context.Background(), ref)
	if err == nil {
		t.Fatal("binary kind should be rejected")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("error should mention binary: %v", err)
	}
}

func TestPullAgent_MissingManifest(t *testing.T) {
	// No handlers — every request 404s.
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()

	rt := &hostRewriter{to: srv.URL, inner: srv.Client().Transport}
	st := store.New(t.TempDir())
	p := &Puller{HTTP: &http.Client{Transport: rt}, Store: st}

	ref := &Ref{Host: "github.com", Owner: "x", Repo: "y", Path: "p", Ref: "main"}
	if _, err := p.PullAgent(context.Background(), ref); err == nil {
		t.Fatal("expected error for missing agent.yaml")
	}
}

// ── helper: redirect raw.githubusercontent.com → httptest server ─────────

type hostRewriter struct {
	to    string // e.g. http://127.0.0.1:NNNN
	inner http.RoundTripper
}

func (h *hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "raw.githubusercontent.com") {
		// Replace scheme+host with test server; keep the path.
		req.URL.Scheme = "http"
		// httptest URL is like http://127.0.0.1:PORT — parse the host.
		i := strings.Index(h.to, "://")
		if i >= 0 {
			req.URL.Host = h.to[i+3:]
		} else {
			req.URL.Host = h.to
		}
	}
	inner := h.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(req)
}
