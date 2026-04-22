package rank

import "testing"

func TestDefaults(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range []string{"intern", "staff", "manager", "director"} {
		if _, err := reg.Get(name); err != nil {
			t.Fatalf("default rank %q missing: %v", name, err)
		}
	}
	if _, err := reg.Get("ceo"); err == nil {
		t.Fatal("unknown rank should error")
	}
}

func TestInternPerms(t *testing.T) {
	rk, _ := DefaultRegistry().Get("intern")
	if !rk.AllowRead("/app/bin/x") {
		t.Fatal("intern should read /app/bin/x")
	}
	if !rk.AllowRead("/tmp/scratch") {
		t.Fatal("intern should read /tmp/scratch")
	}
	if rk.AllowRead("/data/secret") {
		t.Fatal("intern must NOT read /data/secret")
	}
	if !rk.AllowWrite("/tmp/out") {
		t.Fatal("intern should write /tmp/out")
	}
	if rk.AllowWrite("/data/out") {
		t.Fatal("intern must NOT write /data/out")
	}
	if rk.LLMAllowed {
		t.Fatal("intern must NOT have LLM")
	}
	if !rk.NetAllowed {
		t.Fatal("intern should have limited net")
	}
}

func TestStaffPerms(t *testing.T) {
	rk, _ := DefaultRegistry().Get("staff")
	if !rk.LLMAllowed || !rk.NetAllowed {
		t.Fatal("staff should have LLM and net")
	}
	if !rk.AllowRead("/data/anything") {
		t.Fatal("staff should read /data/*")
	}
	if !rk.AllowWrite("/data/out") {
		t.Fatal("staff should write /data/out")
	}
}

func TestDirectorAllowsAllPaths(t *testing.T) {
	rk, _ := DefaultRegistry().Get("director")
	if !rk.AllowRead("/etc/passwd") || !rk.AllowWrite("/etc/somewhere") {
		t.Fatal("director should be unrestricted")
	}
}

func TestCapabilities_Intern(t *testing.T) {
	rk, _ := DefaultRegistry().Get("intern")
	caps := rk.Capabilities()
	// intern has net + fs (read /app /tmp, write /tmp), no LLM
	if !contains(caps, "net") || !contains(caps, "fs") {
		t.Errorf("intern should have net+fs, got %v", caps)
	}
	if contains(caps, "llm") {
		t.Errorf("intern must NOT have llm, got %v", caps)
	}
}

func TestCapabilities_Staff(t *testing.T) {
	rk, _ := DefaultRegistry().Get("staff")
	for _, want := range []string{"net", "llm", "fs"} {
		if !rk.HasCapability(want) {
			t.Errorf("staff should have %s, caps=%v", want, rk.Capabilities())
		}
	}
}

func TestCapabilities_Empty(t *testing.T) {
	rk := &Rank{Name: "sandbox"}
	if len(rk.Capabilities()) != 0 {
		t.Errorf("blank rank should have no caps, got %v", rk.Capabilities())
	}
	if rk.HasCapability("net") {
		t.Error("blank rank shouldn't claim net")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestPrefixBoundary(t *testing.T) {
	// Ensure /data matches /data and /data/x but NOT /database.
	rk := &Rank{FSRead: []string{"/data"}}
	if !rk.AllowRead("/data") {
		t.Fatal("exact match should allow")
	}
	if !rk.AllowRead("/data/sub/file") {
		t.Fatal("deep path should allow")
	}
	if rk.AllowRead("/database/config") {
		t.Fatal("sibling dir must not match")
	}
}
