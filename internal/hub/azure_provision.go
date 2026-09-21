package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// ErrProvisionInFlight is the store's, re-exported so the HTTP layer can map
// it to a 409 without importing the hub.
var ErrProvisionInFlight = store.ErrProvisionInFlight

// provisionTimeout bounds one run. A VM creation is minutes, not seconds, and
// the poll behind it is honest about that; but a run that has been going for
// half an hour has not worked, and leaving it open would keep the name locked
// by the in-flight index forever.
const provisionTimeout = 30 * time.Minute

// StartProvision records the run, then creates the VM off the caller's
// request. It returns as soon as the row exists.
//
// The order here is load-bearing:
//
//  1. Everything that can refuse, refuses first — while nothing exists. A
//     provision the hub will not run must not leave a host row behind.
//  2. The provision row goes in next, because its partial unique index is
//     what refuses a second run of the same name. A check-then-insert is a
//     race two browser tabs win.
//  3. The host row comes last, and before any Azure call, so a VM that boots
//     and dials in finds itself expected rather than rejected.
func (h *Hub) StartProvision(name string) (int64, error) {
	cfg, client, ok := h.azureReady()
	if !ok {
		return 0, ErrAzureOff
	}
	name = strings.TrimSpace(name)
	if err := azure.ValidateVMName(name); err != nil {
		return 0, err
	}
	p := azure.LoadProvisionConfig(h.st)
	if err := p.Ready(); err != nil {
		return 0, err
	}
	parts, err := azure.ParseSubnetID(p.SubnetID)
	if err != nil {
		return 0, err
	}
	if err := withinScope(cfg, parts); err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	provisionID, err := h.st.StartAzureProvision(name, nil, now)
	if err != nil {
		return 0, err
	}
	host, token, err := h.st.CreateHost(name, now)
	if err != nil {
		// The name is taken by a host that already exists, or the write
		// failed. Either way nothing was created in Azure, and the run is
		// closed rather than left holding the name.
		h.finishProvision(provisionID, "failed", err.Error())
		return 0, err
	}
	if err := h.st.SetAzureProvisionHost(provisionID, host.ID); err != nil {
		h.log.Error("azure provision: link host", "err", err)
	}

	go h.runProvision(provisionID, p, azure.CreateRequest{Name: name, HostID: host.ID, Token: token}, client)
	return provisionID, nil
}

// withinScope refuses a subnet the hub does not watch. The invariant is worth
// stating: a VM the hub creates must be a VM the hub can show. Creating one
// in a group nobody syncs would produce a machine that never appears in the
// inventory, never appears in the costs, and is remembered only here.
func withinScope(cfg azure.Config, parts azure.SubnetParts) error {
	if !strings.EqualFold(parts.Subscription, cfg.SubscriptionID) {
		return fmt.Errorf("the subnet is in subscription %s and this hub watches %s",
			parts.Subscription, cfg.SubscriptionID)
	}
	for _, g := range cfg.ResourceGroups {
		if strings.EqualFold(g, parts.ResourceGroup) {
			return nil
		}
	}
	return fmt.Errorf("the subnet is in resource group %s, which this hub does not watch; "+
		"a VM created there would never appear in the inventory or the costs",
		parts.ResourceGroup)
}

// runProvision performs the calls and records what landed. Its context is the
// hub's, not the request's: the request is already over.
func (h *Hub) runProvision(provisionID int64, p azure.ProvisionConfig, req azure.CreateRequest, client *azure.Client) {
	ctx, cancel := context.WithTimeout(h.baseCtx(), provisionTimeout)
	defer cancel()

	if err := h.st.MarkAzureProvisionRunning(provisionID); err != nil {
		h.log.Error("azure provision: mark running", "err", err)
	}

	// Each resource is written down as it lands, before the next call. A
	// record that cannot be written stops the run: the hub must not create
	// something it cannot name afterwards.
	onCreated := func(c azure.Created) error {
		return h.st.RecordAzureProvisionResource(provisionID, c.ARMID, c.Kind, time.Now().UTC())
	}

	if err := azure.CreateVM(ctx, client, p, req, onCreated); err != nil {
		// Nothing is deleted here. What was created is in the record, and the
		// page shows it with a Delete button beside each leftover — see §4 of
		// the lot 5 spec. The error text carries Azure's own reason, and
		// never the token: CreateVM is given it, no error path echoes it.
		h.log.Warn("azure provision failed", "name", req.Name, "err", err)
		h.finishProvision(provisionID, "failed", err.Error())
		return
	}
	h.finishProvision(provisionID, "succeeded", "")
	// The new VM is not in the inventory until a sweep sees it, and the next
	// one may be a quarter of an hour away.
	h.kickAzure()
}

// finishProvision closes the row and publishes the outcome — success and
// failure alike. A failure nobody published was one of lot 3's defects.
func (h *Hub) finishProvision(provisionID int64, status, errMsg string) {
	if err := h.st.FinishAzureProvision(provisionID, status, errMsg, time.Now().UTC()); err != nil {
		h.log.Error("azure provision: finish", "err", err)
	}
	h.bus.Publish("azure_provision", map[string]any{"id": provisionID, "status": status})
}

// interruptProvisions closes what was in flight when the hub died, and never
// resumes it. Unlike an interrupted action, an interrupted provision may have
// created resources that are costing money right now — they stay in the
// record, named, and the page shows them.
func (h *Hub) interruptProvisions() {
	n, err := h.st.InterruptAzureProvisions(time.Now().UTC())
	if err != nil {
		h.log.Error("azure provisions: interrupt", "err", err)
		return
	}
	if n > 0 {
		h.log.Warn("azure provisions were in flight when the hub stopped; they are recorded as "+
			"interrupted and not resumed. Any resource they had already created still exists.",
			"count", n)
	}
}

// ListProvisions is what the page reads.
func (h *Hub) ListProvisions(limit int) ([]store.AzureProvision, error) {
	return h.st.ListAzureProvisions(limit)
}

// ErrWrongName refuses a deletion whose confirmation does not match.
var ErrWrongName = errors.New("azure: the name typed does not match the VM being deleted")

// DeleteProvision removes everything one provision created — the VM, then its
// NIC, the OS disk going with the VM.
//
// It is user-initiated, and confirmed by typing the name rather than by a
// dialog dismissed by reflex: this is the only place in the whole application
// where the hub destroys anything, and the difference between a resource
// created by hand and one created here is a tag nobody can see from the page.
// The tag is checked again in Azure before every DELETE; this check is the
// one that protects against the wrong row being clicked.
func (h *Hub) DeleteProvision(provisionID int64, confirmName string) error {
	_, client, ok := h.azureReady()
	if !ok {
		return ErrAzureOff
	}
	p, err := h.st.AzureProvision(provisionID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirmName) != p.Name {
		return fmt.Errorf("%w: type %q to confirm", ErrWrongName, p.Name)
	}
	var rs []azure.Deletable
	for _, r := range p.Resources {
		if r.DeletedAt == nil {
			rs = append(rs, azure.Deletable{ARMID: r.ARMID, Kind: r.Kind})
		}
	}
	if len(rs) == 0 {
		return nil // everything is already gone; the second press is not a failure
	}
	if err := h.st.SetAzureProvisionDeleteError(provisionID, ""); err != nil {
		h.log.Error("azure provision: clear delete error", "err", err)
	}
	go h.runDeleteProvision(provisionID, rs, client)
	return nil
}

func (h *Hub) runDeleteProvision(provisionID int64, rs []azure.Deletable, client *azure.Client) {
	ctx, cancel := context.WithTimeout(h.baseCtx(), provisionTimeout)
	defer cancel()

	onDeleted := func(d azure.Deletable) error {
		return h.st.MarkAzureProvisionResourceDeleted(d.ARMID, time.Now().UTC())
	}
	if err := azure.DeleteCreated(ctx, client, rs, onDeleted); err != nil {
		h.log.Warn("azure provision delete failed", "provision", provisionID, "err", err)
		if e := h.st.SetAzureProvisionDeleteError(provisionID, err.Error()); e != nil {
			h.log.Error("azure provision: record delete error", "err", e)
		}
	}
	// Published whether it worked or not: a failure nobody published is the
	// defect lot 3 shipped and lot 4 fixed.
	h.bus.Publish("azure_provision", map[string]any{"id": provisionID, "deleted": true})
	h.kickAzure()
}
