package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/room"
	"github.com/anne-x/hive/internal/rpc"
)

// handleHireJunior implements the hire/junior Agent→Hive RPC. The
// caller's Member is `parent` — that's how the daemon knows whose
// rank to compare against and whose quota to carve from.
//
// Flow:
//
//  1. Parse params; resolve image ref; look up requested Rank.
//  2. rank.CanHire enforces "manager+ may hire strictly below".
//  3. If a Quota was requested, atomically carve from parent's bucket.
//     Each `Consume` either reserves the amount or rejects (no partial
//     carve — the call fails as a unit).
//  4. Hand off to hireFromConfig with Parent set, so persistence,
//     proxy install, and recovery all see the lineage.
//  5. Return the child's image name + rank + parent for caller logs.
//
// Quota model: hard carve. The carved amount is "spent" against the
// parent's bucket; if the child uses less, the difference is lost
// rather than auto-refunded. (Refund-on-exit is a v2 papercut.)
func (d *Daemon) handleHireJunior(ctx context.Context, r *room.Room, parent *room.Member, params json.RawMessage) (any, error) {
	var p rpc.HireJuniorParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if p.Ref == "" {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "hire/junior: ref is required")
	}
	if p.Rank == "" {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "hire/junior: rank is required")
	}

	imgRef, err := image.ParseRef(p.Ref)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, fmt.Sprintf("parse ref %q: %v", p.Ref, err))
	}

	childRank, err := d.ranks.Get(p.Rank)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeRankViolation, err.Error())
	}

	// Rank policy: manager+ caller, strictly lower child.
	if err := parent.Rank.CanHire(childRank); err != nil {
		return nil, protocol.NewError(protocol.ErrCodePermissionDenied, err.Error())
	}

	// Optional quota carve. We do this BEFORE hireFromConfig so that on
	// rejection the parent's accounting is untouched — install proxies
	// and quota for the child only after the carve succeeds.
	if p.Quota != nil {
		if err := d.carveQuotaFromParent(ctx, r.ID, parent.Image.Name, p.Quota); err != nil {
			return nil, err
		}
	}

	// Wire-shaped quota for the child's own bucket. A *carved* amount is
	// the child's hard cap — it's the same map we just reserved against
	// the parent. Daemon's installAgentProxies will SetLimit each entry
	// on the child's quota Key.
	var childQuotaRaw json.RawMessage
	if p.Quota != nil {
		raw, err := json.Marshal(ipc.QuotaOverride{
			Tokens:   p.Quota.Tokens,
			APICalls: p.Quota.APICalls,
		})
		if err != nil {
			return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
		}
		childQuotaRaw = raw
	}

	cfg := hireConfig{
		Image:     ipc.ImageRef{Name: imgRef.Name, Version: imgRef.Version},
		RankName:  childRank.Name,
		Model:     p.Model,
		QuotaOver: childQuotaRaw,
		Volumes:   convertHireJuniorVolumes(p.Volumes),
		Parent:    parent.Image.Name,
	}
	child, err := d.hireFromConfig(r, cfg)
	if err != nil {
		// Carve already reserved on the parent; for MVP we don't refund
		// on failure (the carved budget is "spent"). Rare path; clean
		// retry semantics need an actor-level transactional op (v2).
		return nil, err
	}

	// Persist the new member into state.json so daemon-restart recovery
	// brings the subordinate back with its parent link intact.
	d.persistRoom(r)

	return rpc.HireJuniorResult{
		ImageName: child.Image.Name,
		Rank:      child.Rank.Name,
		Parent:    parent.Image.Name,
	}, nil
}

// carveQuotaFromParent atomically reserves the requested amounts on
// the parent's quota buckets. Each entry is a Consume; if any one is
// rejected (insufficient remaining) the whole call fails — but
// previously-Consumed entries from this same call are NOT rolled back
// (the actor doesn't expose a refund op). For MVP this is acceptable:
// callers should request exactly the carve they intend, and rejection
// after partial reservation is rare in practice (it only happens if
// the parent's other concurrent traffic drains a different bucket
// between Consumes).
func (d *Daemon) carveQuotaFromParent(ctx context.Context, roomID, parentImage string, q *rpc.HireJuniorQuota) error {
	for model, amount := range q.Tokens {
		if amount <= 0 {
			continue
		}
		key := quota.Key{RoomID: roomID, Agent: parentImage, Resource: "tokens:" + model}
		res, err := d.quota.Consume(ctx, key, amount)
		if err != nil {
			return protocol.NewError(protocol.ErrCodeInternal, err.Error())
		}
		if res.Unlimited {
			// Parent has no cap on this token bucket — nothing to deduct.
			// We still allow the carve; the child will get its own hard
			// cap, the parent's effective allotment is unchanged.
			continue
		}
		if !res.Allowed {
			return protocol.NewError(protocol.ErrCodeQuotaExceeded,
				fmt.Sprintf("hire/junior: parent quota tokens:%s insufficient (need %d, remaining %d)",
					model, amount, res.Remaining))
		}
	}
	for cat, amount := range q.APICalls {
		if amount <= 0 {
			continue
		}
		key := quota.Key{RoomID: roomID, Agent: parentImage, Resource: cat}
		res, err := d.quota.Consume(ctx, key, amount)
		if err != nil {
			return protocol.NewError(protocol.ErrCodeInternal, err.Error())
		}
		if res.Unlimited {
			continue
		}
		if !res.Allowed {
			return protocol.NewError(protocol.ErrCodeQuotaExceeded,
				fmt.Sprintf("hire/junior: parent quota %s insufficient (need %d, remaining %d)",
					cat, amount, res.Remaining))
		}
	}
	return nil
}

// refundCarvesToParent flows unused subordinate quota back into the
// parent's bucket — the inverse of carveQuotaFromParent. Called from
// the OnAgentExit hook for any Member with non-empty Parent.
//
// Semantics: at hire time we Consume'd the parent's bucket by the
// child's full carved amount. At exit time we know how much the child
// actually used (consumed) — the unused remainder is the refund. The
// child's quota.Actor entries are about to be torn down so reading
// them now via Remaining gives a stable snapshot.
//
// We iterate the child's effective quota maps to know which keys to
// refund. For each one with a configured limit (skipping director-
// style unlimited buckets), we Uncharge the parent the unused amount.
// Pure read-then-write under the actor's serialisation; no atomicity
// concerns across multiple buckets because each is independent.
func (d *Daemon) refundCarvesToParent(roomID string, child *room.Member) {
	if child.Parent == "" {
		return
	}
	ctx := d.quotaCtx
	eq := child.EffectiveQuota()
	for model, _ := range eq.Tokens {
		resource := "tokens:" + model
		childKey := quota.Key{RoomID: roomID, Agent: child.Image.Name, Resource: resource}
		parentKey := quota.Key{RoomID: roomID, Agent: child.Parent, Resource: resource}
		d.refundOneBucket(ctx, childKey, parentKey)
	}
	for cat := range eq.APICalls {
		childKey := quota.Key{RoomID: roomID, Agent: child.Image.Name, Resource: cat}
		parentKey := quota.Key{RoomID: roomID, Agent: child.Parent, Resource: cat}
		d.refundOneBucket(ctx, childKey, parentKey)
	}
}

// refundOneBucket reads the child's remaining for `childKey` and
// uncharges that amount from `parentKey`. Defensive against either
// key being unlimited (no limit installed) — both cases just no-op
// since "refund unused" is meaningless when neither side has a cap.
func (d *Daemon) refundOneBucket(ctx context.Context, childKey, parentKey quota.Key) {
	res, err := d.quota.Remaining(ctx, childKey)
	if err != nil || res.Unlimited || res.Remaining <= 0 {
		return
	}
	if _, err := d.quota.Uncharge(ctx, parentKey, res.Remaining); err != nil {
		// Not fatal — the child has already exited and we can't really
		// recover. Log so an operator can see the leak if it ever
		// happens.
		log.Printf("hire/junior refund: uncharge %v by %d failed: %v", parentKey, res.Remaining, err)
		return
	}
	// Success: log at info level so daemon.log shows the refund as a
	// concrete line ("paper-style-critic returned 3628 tokens to
	// paper-supervisor on tokens:openai/gpt-5.4-mini"). Cheap insight
	// for operators wondering why a parent's bucket grew unexpectedly.
	log.Printf("hire/junior refund: %s → %s on %s: %d", childKey.Agent, parentKey.Agent, parentKey.Resource, res.Remaining)
}

// convertHireJuniorVolumes shifts from the rpc package's wire-shape
// (avoids importing internal/ipc) to the ipc shape that hireFromConfig
// already understands.
func convertHireJuniorVolumes(vs []rpc.HireJuniorVolumeMount) []ipc.VolumeMountRef {
	if len(vs) == 0 {
		return nil
	}
	out := make([]ipc.VolumeMountRef, len(vs))
	for i, v := range vs {
		out[i] = ipc.VolumeMountRef{
			Name:       v.Name,
			Mode:       v.Mode,
			Mountpoint: v.Mountpoint,
		}
	}
	return out
}
