// Package rank defines the Rank model: permission tier + default quotas.
//
// A Rank sits on top of the OS-level sandbox (namespaces) and adds semantic
// permissions: what kind of I/O the Agent may request through Hive's proxies.
// Rank checks happen inside the proxy handlers — one place for the whole
// permission model, as ARCHITECTURE.md §113 requires.
package rank

import (
	"fmt"
	"strings"
)

// Rank is a tier of permissions + default quotas.
//
// For demo scope: we keep the six-category split from ARCHITECTURE.md but
// only populate fields that the demo proxies actually consult. Everything
// unused is zero-valued and ignored.
type Rank struct {
	Name string

	// Level orders Ranks for "manager hires intern" semantics. Higher
	// number = more senior. The default registry assigns
	// intern=0, staff=1, manager=2, director=3. Auto-hire policy
	// (CanHire) only allows hiring strictly below: a manager can hire
	// staff or intern but not another manager — this rules out
	// peer-cycle delegation and keeps the subordinate tree, well, a tree.
	Level int

	// FS: prefix-based allow lists. Empty means "nothing allowed".
	// Paths are as the Agent sees them (post-pivot_root), always absolute,
	// and must start with one of the prefixes to be allowed.
	FSRead  []string
	FSWrite []string

	// Net: whether the Agent may call net/fetch at all. Domain-level
	// allow/deny is not in demo scope (would need DNS hooking in proxy).
	NetAllowed bool

	// LLM: whether the Agent may call llm/complete.
	LLMAllowed bool

	// Memory: whether the Agent may use memory/put/get/list/delete.
	// Binary gate; volume-level ACL is future work.
	MemoryAllowed bool

	// AITool: whether the Agent may call ai_tool/invoke (Claude Code CLI
	// or any registered ai-tool Provider). Binary gate; per-tool ACL is
	// future work — see the aitoolproxy package.
	AIToolAllowed bool

	// HireAllowed: whether the Agent may call hire/junior to spawn a
	// subordinate Agent at runtime. Defaults to false; manager and
	// director are the only built-in Ranks that grant it. Ranks below
	// can never auto-hire (an intern that wants help has to peer/send
	// out and wait for someone else to hire — by design).
	HireAllowed bool

	// Default quotas; a Hire-time override may raise or lower these.
	Quota Quota
}

// CanHire reports whether self may auto-hire `child` as a subordinate.
// Three rules:
//
//  1. self.HireAllowed must be true (intern/staff cannot auto-hire).
//  2. child.Level must be strictly less than self.Level (no peer or
//     upward hires — keeps the subordinate graph a tree).
//  3. self.Level >= child.Level rule is implied by rule 2.
//
// Returns a descriptive error when denied so the daemon can surface it
// to the caller as a permission_denied with a useful message.
func (self *Rank) CanHire(child *Rank) error {
	if self == nil || child == nil {
		return fmt.Errorf("rank: nil rank in CanHire")
	}
	if !self.HireAllowed {
		return fmt.Errorf("rank %q is not allowed to hire (HireAllowed=false)", self.Name)
	}
	if child.Level >= self.Level {
		return fmt.Errorf("rank %q (level %d) cannot hire rank %q (level %d): subordinate must be strictly lower",
			self.Name, self.Level, child.Name, child.Level)
	}
	return nil
}

// Quota caps are hard maximums per (Room, Agent) pair.
type Quota struct {
	// Tokens: key is the model name (e.g. "gpt-4o-mini"),
	// value is the maximum total tokens this Agent may consume. The actual
	// provider serving the model (openai / mock / anthropic) doesn't enter
	// the key — it's an implementation detail the daemon owns, while the
	// Rank models "how much work on this model the Agent may do".
	Tokens map[string]int

	// APICalls: key is an endpoint category (e.g. "http"),
	// value is the maximum number of calls allowed.
	APICalls map[string]int
}

// AllowRead reports whether the Rank permits reading absPath.
// absPath must be absolute and already cleaned by the caller.
func (r *Rank) AllowRead(absPath string) bool { return hasPrefix(r.FSRead, absPath) }

// AllowWrite reports whether the Rank permits writing absPath.
func (r *Rank) AllowWrite(absPath string) bool { return hasPrefix(r.FSWrite, absPath) }

// Capabilities returns the Hive-defined capability tokens this Rank
// effectively grants. These are the vocabulary for manifest.capabilities
// (requires/provides). An Agent whose manifest.capabilities.requires
// contains a token NOT in the Rank's set is rejected at hire time.
//
// Current vocabulary:
//   "net"     — Rank.NetAllowed
//   "llm"     — Rank.LLMAllowed
//   "fs"      — Rank has at least one FS read OR write prefix
//   "memory"  — Rank.MemoryAllowed (memory/put/get/list/delete)
//   "ai_tool" — Rank.AIToolAllowed (ai_tool/invoke — Claude Code et al.)
func (r *Rank) Capabilities() []string {
	var caps []string
	if r.NetAllowed {
		caps = append(caps, "net")
	}
	if r.LLMAllowed {
		caps = append(caps, "llm")
	}
	if len(r.FSRead) > 0 || len(r.FSWrite) > 0 {
		caps = append(caps, "fs")
	}
	if r.MemoryAllowed {
		caps = append(caps, "memory")
	}
	if r.AIToolAllowed {
		caps = append(caps, "ai_tool")
	}
	return caps
}

// HasCapability reports whether the Rank grants the given capability token.
func (r *Rank) HasCapability(cap string) bool {
	for _, c := range r.Capabilities() {
		if c == cap {
			return true
		}
	}
	return false
}

func hasPrefix(prefixes []string, p string) bool {
	for _, pref := range prefixes {
		if pref == "/" {
			return true
		}
		if p == pref || strings.HasPrefix(p, strings.TrimSuffix(pref, "/")+"/") {
			return true
		}
	}
	return false
}

// Registry holds all known Ranks.
type Registry struct {
	ranks map[string]*Rank
}

// DefaultRegistry returns the four built-in Ranks from ARCHITECTURE.md.
// Intern/staff are conservative (no LLM/net for intern); manager adds
// larger quotas; director is effectively unconstrained.
func DefaultRegistry() *Registry {
	r := &Registry{ranks: make(map[string]*Rank)}
	// intern mirrors ARCHITECTURE.md §114: API-only, narrow read, no LLM, low quota.
	// We grant a small number of HTTP calls so intern-ranked Agents can do
	// search/data-gathering duties (the canonical arxiv-search use case).
	r.ranks["intern"] = &Rank{
		Name:       "intern",
		Level:      0,
		FSRead:     []string{"/app", "/tmp"},
		FSWrite:    []string{"/tmp"},
		NetAllowed: true,
		Quota: Quota{
			APICalls: map[string]int{"http": 5},
		},
	}
	r.ranks["staff"] = &Rank{
		Name:          "staff",
		Level:         1,
		FSRead:        []string{"/app", "/tmp", "/data"},
		FSWrite:       []string{"/tmp", "/data"},
		NetAllowed:    true,
		LLMAllowed:    true,
		MemoryAllowed: true,
		AIToolAllowed: true,
		Quota: Quota{
			Tokens:   map[string]int{"gpt-4o-mini": 5000},
			APICalls: map[string]int{"http": 20, "ai_tool:claude-code": 10},
		},
	}
	r.ranks["manager"] = &Rank{
		Name:          "manager",
		Level:         2,
		FSRead:        []string{"/"},
		FSWrite:       []string{"/tmp", "/data"},
		NetAllowed:    true,
		LLMAllowed:    true,
		MemoryAllowed: true,
		AIToolAllowed: true,
		HireAllowed:   true,
		Quota: Quota{
			Tokens:   map[string]int{"gpt-4o-mini": 50000},
			APICalls: map[string]int{"http": 200, "ai_tool:claude-code": 100},
		},
	}
	r.ranks["director"] = &Rank{
		Name:          "director",
		Level:         3,
		FSRead:        []string{"/"},
		FSWrite:       []string{"/"},
		NetAllowed:    true,
		LLMAllowed:    true,
		MemoryAllowed: true,
		AIToolAllowed: true,
		HireAllowed:   true,
		// Director has unlimited quota; we signal that by leaving maps nil,
		// and the proxy layer treats "no limit entry" as "unlimited". See
		// quota.Actor.Consume.
	}
	return r
}

// Get looks up a Rank by name.
func (r *Registry) Get(name string) (*Rank, error) {
	rk, ok := r.ranks[name]
	if !ok {
		return nil, fmt.Errorf("unknown rank: %q", name)
	}
	return rk, nil
}

// List returns all Rank names in registration order (for CLI display).
func (r *Registry) List() []string {
	out := make([]string, 0, len(r.ranks))
	for _, o := range []string{"intern", "staff", "manager", "director"} {
		if _, ok := r.ranks[o]; ok {
			out = append(out, o)
		}
	}
	return out
}
