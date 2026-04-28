package daemon

import (
	"context"
	"testing"

	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rpc"
)

// TestCarveQuotaFromParent covers the three branches in
// carveQuotaFromParent: success with limits, success when the parent
// has no cap on the bucket (unlimited passthrough), and rejection when
// remaining is below the request.
func TestCarveQuotaFromParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := quota.New(0)
	go q.Run(ctx)

	d := &Daemon{quota: q}

	// Seed parent with a 1000-token cap on gpt-4o-mini and 50 http calls.
	roomID, parent := "room-A", "manager-1"
	if _, err := q.SetLimit(ctx, quota.Key{RoomID: roomID, Agent: parent, Resource: "tokens:gpt-4o-mini"}, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetLimit(ctx, quota.Key{RoomID: roomID, Agent: parent, Resource: "http"}, 50); err != nil {
		t.Fatal(err)
	}

	// 1. Successful carve: 300 tokens + 10 http. Remaining should drop.
	err := d.carveQuotaFromParent(ctx, roomID, parent, &rpc.HireJuniorQuota{
		Tokens:   map[string]int{"gpt-4o-mini": 300},
		APICalls: map[string]int{"http": 10},
	})
	if err != nil {
		t.Fatalf("carve 300+10: %v", err)
	}
	res, _ := q.Remaining(ctx, quota.Key{RoomID: roomID, Agent: parent, Resource: "tokens:gpt-4o-mini"})
	if res.Remaining != 700 {
		t.Errorf("after first carve tokens remaining = %d want 700", res.Remaining)
	}
	res, _ = q.Remaining(ctx, quota.Key{RoomID: roomID, Agent: parent, Resource: "http"})
	if res.Remaining != 40 {
		t.Errorf("after first carve http remaining = %d want 40", res.Remaining)
	}

	// 2. Rejected carve: parent only has 700 tokens left, asks for 800.
	err = d.carveQuotaFromParent(ctx, roomID, parent, &rpc.HireJuniorQuota{
		Tokens: map[string]int{"gpt-4o-mini": 800},
	})
	if err == nil {
		t.Fatal("expected rejection on over-budget carve")
	}

	// 3. Unlimited bucket — parent has NO cap on a different model.
	// Carving against it must succeed without affecting anything.
	err = d.carveQuotaFromParent(ctx, roomID, parent, &rpc.HireJuniorQuota{
		Tokens: map[string]int{"openai/gpt-5.4-mini": 9999},
	})
	if err != nil {
		t.Errorf("unlimited bucket carve: %v", err)
	}
	// And the limited bucket should still be at 700 — rejection (#2)
	// shouldn't have decremented anything.
	res, _ = q.Remaining(ctx, quota.Key{RoomID: roomID, Agent: parent, Resource: "tokens:gpt-4o-mini"})
	if res.Remaining != 700 {
		t.Errorf("rejection mutated state: tokens remaining = %d", res.Remaining)
	}
}

// TestConvertHireJuniorVolumes is a tiny shape-conversion test — exists
// to lock the field mapping so a future rename in either rpc or ipc
// surfaces as a compile/test error rather than a silent data loss.
func TestConvertHireJuniorVolumes(t *testing.T) {
	in := []rpc.HireJuniorVolumeMount{
		{Name: "shared", Mode: "ro", Mountpoint: "/data"},
		{Name: "draft", Mode: "rw", Mountpoint: "/draft"},
	}
	out := convertHireJuniorVolumes(in)
	if len(out) != 2 {
		t.Fatalf("len = %d want 2", len(out))
	}
	if out[0].Name != "shared" || out[0].Mode != "ro" || out[0].Mountpoint != "/data" {
		t.Errorf("[0] = %+v", out[0])
	}
	if out[1].Name != "draft" || out[1].Mode != "rw" || out[1].Mountpoint != "/draft" {
		t.Errorf("[1] = %+v", out[1])
	}
	if convertHireJuniorVolumes(nil) != nil {
		t.Error("nil → should be nil")
	}
	if convertHireJuniorVolumes([]rpc.HireJuniorVolumeMount{}) != nil {
		t.Error("empty → should be nil (no allocation)")
	}
}
