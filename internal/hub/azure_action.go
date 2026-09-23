package hub

import (
	"context"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// Aliases: the sentinels live in the azure package so the HTTP layer can map
// them without importing the hub.
var (
	ErrAzureOff      = azure.ErrNotConfigured
	ErrNotActionable = azure.ErrNotActionable
)

// StartAction records the action, then runs it off the caller's request. It
// returns as soon as the row exists: a restart takes tens of seconds, and an
// HTTP request held open that long dies in a proxy and leaves the browser
// believing a success failed.
func (h *Hub) StartAction(resourceID string, a azure.Action) (int64, error) {
	return h.StartActionFrom(resourceID, a, "user")
}

// StartActionFrom is StartAction with an origin. Schedules pass "schedule";
// the row says so, and the failure of a scheduled action becomes a guardrail
// event rather than a line only the action log knows.
func (h *Hub) StartActionFrom(resourceID string, a azure.Action, origin string) (int64, error) {
	_, client, ok := h.azureReady()
	if !ok {
		return 0, ErrAzureOff
	}
	id := azure.NormalizeID(resourceID)

	// The inventory is the allow-list. A resource the hub cannot show is one it
	// does not act on, whatever the browser sent, and a resource Azure no
	// longer has is not actionable either.
	r, err := h.st.ActionableAzureResource(id)
	if err != nil {
		return 0, err
	}
	if !azure.Supports(r.Type, a) {
		return 0, ErrNotActionable
	}

	actionID, err := h.st.StartAzureAction(store.AzureAction{
		ResourceID: id, ResourceName: r.Name, Action: string(a),
		RequestedAt: time.Now().UTC(), StateBefore: r.State, Origin: origin,
	})
	if err != nil {
		return 0, err
	}
	go h.runAction(actionID, r, a, client, origin)
	return actionID, nil
}

// runAction performs the call and records what Azure answered. Its context is
// the hub's, not the request's: the request is already over.
func (h *Hub) runAction(actionID int64, r store.AzureResource, a azure.Action, client *azure.Client, origin string) {
	ctx, cancel := context.WithTimeout(h.baseCtx(), 10*time.Minute)
	defer cancel()

	if err := h.st.MarkAzureActionRunning(actionID); err != nil {
		h.log.Error("azure action: mark running", "err", err)
	}

	armID := r.ARMID
	if armID == "" {
		armID = r.ID // written before arm_id existed; the next sync fills it in
	}

	if err := azure.Do(ctx, client, armID, r.Type, a); err != nil {
		h.log.Warn("azure action failed", "resource", r.Name, "action", a, "err", err)
		h.finishAction(actionID, "failed", err.Error(), nil, r, origin)
		return
	}

	// The state is read, never inferred from the action requested. A read that
	// fails leaves the inventory exactly as it was: never a wrong certainty.
	//
	// It is budgeted. A read is safe to repeat, so the client retries a 5xx for
	// up to half a minute — but the action row stays open until this returns,
	// and the page would show "running" long after the stop went through. When
	// the budget runs out the periodic sync is what settles the state.
	rctx, rcancel := context.WithTimeout(ctx, h.readBack)
	defer rcancel()
	state, err := azure.ReadState(rctx, client, armID, r.Type)
	if err != nil {
		h.log.Warn("azure action: state read-back failed", "resource", r.Name, "err", err)
		h.finishAction(actionID, "succeeded", "", nil, r, origin)
		return
	}
	if state != nil {
		if err := h.st.SetAzureResourceState(r.ID, *state); err != nil {
			h.log.Error("azure action: store state", "err", err)
		}
	}
	h.finishAction(actionID, "succeeded", "", state, r, origin)
}

// finishAction closes the row and publishes the outcome — success and failure
// alike. A failure nobody published was one of lot 3's defects.
//
// For a scheduled action (origin == "schedule") it also fires or resolves
// schedule_failed: a scheduled action that fails after its row exists is the
// twin of runSchedules' journalScheduleFailure call for a boundary that never
// got a row at all (the resource left the inventory, or stopped being
// actionable) — together they are the two places a boundary can go wrong.
func (h *Hub) finishAction(actionID int64, status, errMsg string, state *string, r store.AzureResource, origin string) {
	if err := h.st.FinishAzureAction(actionID, status, errMsg, state, time.Now().UTC()); err != nil {
		h.log.Error("azure action: finish", "err", err)
	}
	h.bus.Publish("azure_action", map[string]any{"id": actionID, "status": status})
	if origin == "schedule" {
		if status == "failed" {
			h.journalScheduleFailure(r.ID, errMsg)
		} else {
			h.journalScheduleResolved(r.ID)
		}
	}
}

// interruptActions closes what was in flight when the hub died. It never
// replays: re-firing a stop could stop a resource restarted by hand in the
// meantime. Deliveries replay because a lost alert is worse than a duplicate;
// an action is the opposite, and this asymmetry is deliberate.
func (h *Hub) interruptActions() {
	n, err := h.st.InterruptAzureActions(time.Now().UTC())
	if err != nil {
		h.log.Error("azure actions: interrupt", "err", err)
		return
	}
	if n > 0 {
		h.log.Warn("azure actions were in flight when the hub stopped; "+
			"they are recorded as interrupted and not replayed", "count", n)
	}
}
