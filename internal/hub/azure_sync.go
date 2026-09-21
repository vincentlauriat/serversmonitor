package hub

import (
	"context"
	"errors"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// currentPeriod is the month a cost row belongs to.
func currentPeriod(now time.Time) string { return now.UTC().Format("2006-01") }

// ReloadAzure rebuilds the client from the settings. Called at boot and
// whenever the Azure settings are saved.
//
// Changing the scope purges what was collected for the old one: resources from
// a group nobody watches any more would otherwise linger forever, never swept
// because never synced.
func (h *Hub) ReloadAzure() {
	c := azure.LoadConfig(h.st)
	if prev := h.acfg.Load(); prev != nil && scopeChanged(*prev, c) {
		if err := h.st.PurgeAzure(); err != nil {
			h.log.Error("purge azure after a scope change", "err", err)
		}
	}
	h.acfg.Store(&c)
	if !c.Enabled() {
		h.aclient.Store(nil)
		return
	}
	src, err := azure.NewSource(c, nil)
	if err != nil {
		h.log.Error("azure source", "err", err)
		h.aclient.Store(nil)
		return
	}
	if h.azureBase != "" { // tests point ARM and the token endpoint at one fake
		if cached, ok := src.(*azure.Cached); ok {
			cached.WithEndpointForTests(h.azureBase)
		}
	}
	h.aclient.Store(azure.NewClient(src, azure.Options{Base: h.azureBase, Sleep: h.azureSleep}))
	h.kickAzure()
}

// kickAzure asks the run loop to sync now. Never blocks: one pending kick is
// enough, and a reload that waited on the loop would deadlock the save.
func (h *Hub) kickAzure() {
	select {
	case h.akick <- struct{}{}:
	default:
	}
}

func scopeChanged(a, b azure.Config) bool {
	if a.SubscriptionID != b.SubscriptionID || len(a.ResourceGroups) != len(b.ResourceGroups) {
		return true
	}
	for i := range a.ResourceGroups {
		if a.ResourceGroups[i] != b.ResourceGroups[i] {
			return true
		}
	}
	return false
}

// azureReady returns the config and client, or false when Azure is off.
func (h *Hub) azureReady() (azure.Config, *azure.Client, bool) {
	cp := h.acfg.Load()
	cl := h.aclient.Load()
	if cp == nil || cl == nil || !cp.Enabled() {
		return azure.Config{}, nil, false
	}
	return *cp, cl, true
}

// goSyncAzure runs both sweeps off the run loop. Never inline: an Azure read
// takes minutes when the credential endpoint is unreachable, and Run is one
// goroutine — held there, the hub evaluates no rules and dispatches no
// notifications for the whole of it. A ticker buffers one tick, so those
// minutes are dropped rather than caught up.
func (h *Hub) goSyncAzure(ctx context.Context) {
	h.goSyncAzureInventory(ctx)
	h.goSyncAzureCosts(ctx)
}

// goSyncAzureInventory starts a sweep unless one is already running. A second
// concurrent sweep is worse than a skipped one: the stale view of whichever
// finishes last would mark the other's fresh rows deleted.
func (h *Hub) goSyncAzureInventory(ctx context.Context) {
	if !h.ainv.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer h.ainv.Store(false)
		h.syncAzureInventory(ctx)
	}()
}

func (h *Hub) goSyncAzureCosts(ctx context.Context) {
	if !h.acost.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer h.acost.Store(false)
		h.syncAzureCosts(ctx)
	}()
}

// syncAzureInventory replaces the inventory, but only when every call in the
// sweep succeeded. A partial read would be swept as a batch of deletions, and
// "I cannot see Azure" would render as "the sandbox is empty".
func (h *Hub) syncAzureInventory(ctx context.Context) {
	cfg, client, ok := h.azureReady()
	if !ok {
		return
	}
	started := time.Now().UTC()
	rs, err := azure.Inventory(ctx, client, cfg.SubscriptionID, cfg.ResourceGroups)
	if err != nil {
		h.log.Warn("azure inventory sync failed", "err", err)
		h.recordAzureSync("inventory", false, err.Error(), started)
		return
	}
	now := time.Now().UTC()
	rows := make([]store.AzureResource, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, store.AzureResource{ID: r.ID, ARMID: r.ARMID, Name: r.Name, Type: r.Type,
			ResourceGroup: r.ResourceGroup, Location: r.Location, Kind: r.Kind, SKU: r.SKU,
			State: r.State, ProvisioningState: r.ProvisioningState, Host: r.Host, Tags: r.Tags})
	}
	if err := h.st.ReplaceAzureInventory(cfg.ResourceGroups, rows, now); err != nil {
		h.log.Error("store azure inventory", "err", err)
		h.recordAzureSync("inventory", false, err.Error(), started)
		return
	}
	h.recordAzureSync("inventory", true, "", started)
}

func (h *Hub) syncAzureCosts(ctx context.Context) {
	cfg, client, ok := h.azureReady()
	if !ok {
		return
	}
	started := time.Now().UTC()
	cs, err := azure.Costs(ctx, client, cfg.SubscriptionID, cfg.ResourceGroups)
	if err != nil {
		h.log.Warn("azure cost sync failed", "err", err)
		h.recordAzureSync("cost", false, err.Error(), started)
		return
	}
	now := time.Now().UTC()
	period := currentPeriod(now)
	rows := make([]store.AzureCost, 0, len(cs))
	for _, c := range cs {
		// No filtering against the inventory: a resource deleted mid-month
		// still cost money, and dropping its row understates the bill.
		rows = append(rows, store.AzureCost{ResourceID: c.ResourceID, Period: period,
			Amount: c.Amount, Currency: c.Currency, AsOf: now})
	}
	if err := h.st.UpsertAzureCosts(rows); err != nil {
		h.log.Error("store azure costs", "err", err)
		h.recordAzureSync("cost", false, err.Error(), started)
		return
	}
	h.recordAzureSync("cost", true, "", started)
}

// recordAzureSync writes the outcome and tells the open pages about it —
// every outcome, not only the good one. An Azure read can take minutes when the
// credential endpoint is unreachable, and a page that only learns about
// successes sits on "no inventory has run yet" for the whole of a failure.
func (h *Hub) recordAzureSync(scope string, ok bool, msg string, started time.Time) {
	if err := h.st.SetAzureSync(store.AzureSync{Scope: scope, OK: ok, Message: msg,
		StartedAt: started, EndedAt: time.Now().UTC()}); err != nil {
		h.log.Error("record azure sync", "err", err)
	}
	h.bus.Publish("azure", map[string]any{"scope": scope, "ok": ok})
}

// TestAzure acquires a token and runs one catalogue call, returning Azure's own
// error. It does not go through the periodic job: the button must answer now,
// with the reason, not four retries later in a log.
func (h *Hub) TestAzure(ctx context.Context) error {
	cfg, client, ok := h.azureReady()
	if !ok {
		return errors.New("azure is off; choose a mode and save first")
	}
	_, err := azure.Inventory(ctx, client, cfg.SubscriptionID, cfg.ResourceGroups[:1])
	return err
}

// azureInterval reads the configured cadence, falling back to a long one when
// Azure is off so a disabled integration costs one wake-up an hour.
func (h *Hub) azureInterval(scope string) time.Duration {
	c := h.acfg.Load()
	if c == nil || !c.Enabled() {
		return time.Hour
	}
	if scope == "cost" {
		return time.Duration(c.CostEveryMin) * time.Minute
	}
	return time.Duration(c.InventoryEveryMin) * time.Minute
}
