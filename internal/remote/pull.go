package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/store"
)

// Puller fetches Agent sources from a Ref and installs them in a local
// store. One instance is daemon-wide — the embedded http.Client pools
// connections to raw.githubusercontent.com across Rooms and Agents.
type Puller struct {
	HTTP  *http.Client
	Store *store.Store
}

// NewPuller builds a Puller with a sane 30s HTTP timeout. Callers can
// swap HTTP for a test-friendly one (see pull_test.go).
func NewPuller(s *store.Store) *Puller {
	return &Puller{
		HTTP:  &http.Client{Timeout: 30 * time.Second},
		Store: s,
	}
}

// PullAgent fetches the Agent manifest at ref + whatever files its Kind
// requires (e.g. SKILL.md for kind=skill), writes them into a temp
// staging dir, validates the manifest, and imports into the store.
//
// kind=binary is deliberately rejected: hosting and running a platform-
// specific executable from a random repo is a much bigger trust step
// than text-based Agents. Users who want kind=binary build locally.
func (p *Puller) PullAgent(ctx context.Context, ref *Ref) (*image.Image, error) {
	stage, err := os.MkdirTemp("", "hive-pull-*")
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	defer os.RemoveAll(stage)

	// 1. agent.yaml (authoritative manifest)
	manifestBytes, err := p.fetch(ctx, ref.RawURL("agent.yaml"))
	if err != nil {
		return nil, fmt.Errorf("fetch agent.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "agent.yaml"), manifestBytes, 0o644); err != nil {
		return nil, err
	}

	// 2. Parse to learn Kind (we don't call image.LoadManifest yet because
	//    Validate would fail for kind=skill before SKILL.md is on disk).
	var m image.Manifest
	if err := yaml.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("parse agent.yaml: %w", err)
	}
	if m.Kind == "" {
		m.Kind = image.KindBinary
	}

	// 3. Kind-specific payload.
	switch m.Kind {
	case image.KindSkill:
		if m.Skill == "" {
			return nil, fmt.Errorf("remote: agent.yaml kind=skill but skill: field missing")
		}
		skillBytes, err := p.fetch(ctx, ref.RawURL(m.Skill))
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", m.Skill, err)
		}
		dst := filepath.Join(stage, m.Skill)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, skillBytes, 0o644); err != nil {
			return nil, err
		}

	case image.KindWorkflow:
		switch {
		case m.Workflow != "":
			b, err := p.fetch(ctx, ref.RawURL(m.Workflow))
			if err != nil {
				return nil, fmt.Errorf("fetch %s: %w", m.Workflow, err)
			}
			dst := filepath.Join(stage, m.Workflow)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				return nil, err
			}
		case m.Planner != "":
			b, err := p.fetch(ctx, ref.RawURL(m.Planner))
			if err != nil {
				return nil, fmt.Errorf("fetch %s: %w", m.Planner, err)
			}
			dst := filepath.Join(stage, m.Planner)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("remote: kind=workflow requires workflow: or planner:")
		}

	case image.KindBinary:
		return nil, fmt.Errorf(
			"remote pull not supported for kind=binary — build locally or publish as kind=skill/workflow (see README §TODO)")

	default:
		return nil, fmt.Errorf("unknown manifest.kind: %q", m.Kind)
	}

	// 4. Hand the staged dir to the store, which re-parses + re-validates.
	return p.Store.Put(stage)
}

// FetchHivefile pulls a raw Hivefile.yaml from the ref and returns its
// bytes. It does NOT parse — that's the hivefile package's job — because
// this layer wants to stay focused on transport.
func (p *Puller) FetchHivefile(ctx context.Context, ref *Ref) ([]byte, error) {
	return p.fetch(ctx, ref.RawURL("Hivefile.yaml"))
}

func (p *Puller) fetch(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hive-daemon")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
