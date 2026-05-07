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

// TestRefundOneBucket covers the per-bucket refund-on-exit path. Three
// scenarios: child used some (refund the rest), child used everything
// (no-op), child key missing entirely (treated as unlimited → no-op).
func TestRefundOneBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := quota.New(0)
	go q.Run(ctx)

	d := &Daemon{quota: q, quotaCtx: ctx}

	roomID := "R"
	parentKey := quota.Key{RoomID: roomID, Agent: "manager", Resource: "tokens:gpt"}
	childKey := quota.Key{RoomID: roomID, Agent: "intern", Resource: "tokens:gpt"}

	// Set up: parent had 1000, carved 600 to child. Parent consumed=600,
	// remaining=400. Child has its own bucket of 600, consumed 350 (used
	// some of the carved budget), remaining=250.
	q.SetLimit(ctx, parentKey, 1000)
	q.Consume(ctx, parentKey, 600) // simulates carve
	q.SetLimit(ctx, childKey, 600)
	q.Consume(ctx, childKey, 350)

	// 1. Child exits with 250 unused → refund 250 to parent.
	d.refundOneBucket(ctx, childKey, parentKey)
	res, _ := q.Remaining(ctx, parentKey)
	if res.Remaining != 650 {
		t.Errorf("after refund parent.remaining=%d want 650", res.Remaining)
	}

	// 2. Calling refund again on the same child key (which still has
	// remaining=250 — we don't reset it) would double-refund. Daemon
	// only calls refundOneBucket once per exit, so this isn't a real
	// path; but the behaviour-on-bug is "uncharge clamps at zero".
	d.refundOneBucket(ctx, childKey, parentKey)
	res, _ = q.Remaining(ctx, parentKey)
	if res.Remaining != 900 {
		t.Errorf("after double-refund parent.remaining=%d want 900 (still <= 1000 limit)", res.Remaining)
	}

	// 3. Child bucket missing (never had a limit installed) → unlimited
	// → refund no-ops, parent unchanged. Use a fresh parent key so this
	// case stands alone independent of the previous mutations.
	freshParent := quota.Key{RoomID: roomID, Agent: "manager-2", Resource: "tokens:gpt"}
	q.SetLimit(ctx, freshParent, 1000)
	q.Consume(ctx, freshParent, 200)
	missingChild := quota.Key{RoomID: roomID, Agent: "ghost", Resource: "tokens:gpt"}
	d.refundOneBucket(ctx, missingChild, freshParent)
	res, _ = q.Remaining(ctx, freshParent)
	if res.Remaining != 800 {
		t.Errorf("ghost-child refund mutated parent: remaining=%d want 800", res.Remaining)
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
