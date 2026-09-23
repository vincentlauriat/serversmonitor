package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/guardrails"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// Azurer is the hub seen from the server: reload the client after a save, and
// run one connection test on demand. An interface rather than the hub itself,
// so the server does not import the package that imports it.
type Azurer interface {
	ReloadAzure()
	TestAzure(ctx context.Context) error
	// StartAction records the action and runs it off this request, returning
	// the id of the row it wrote.
	StartAction(resourceID string, action azure.Action) (int64, error)
	// StartProvision records the run and creates the VM off this request.
	StartProvision(name string) (int64, error)
	// DeleteProvision removes what one run created, after the name has been
	// typed back. It is the only destructive call in the whole API.
	DeleteProvision(provisionID int64, confirmName string) error
	// GuardrailSettings is the four settings of lot 6, read live: there is
	// nothing to reload, so unlike ReloadAzure this is called on every read.
	GuardrailSettings() guardrails.Settings
	// DeleteOrphan is lot 5's delete-by-name generalised to an ARM id and
	// type. The second and last destructive call the hub makes.
	DeleteOrphan(resourceID, confirmName string) error
}

type azureRow struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Group    string            `json:"resource_group"`
	Location string            `json:"location"`
	State    *string           `json:"state"` // null, never "unknown"
	Host     string            `json:"host,omitempty"`
	Tags     map[string]string `json:"tags"`
	Cost     *float64          `json:"cost"` // null when Azure has not reported
	Currency string            `json:"currency,omitempty"`
	Deleted  bool              `json:"deleted"`
}

type azureTotal struct {
	Currency string  `json:"currency"`
	Spent    float64 `json:"spent"`
}

type syncView struct {
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type azureView struct {
	Mode   string       `json:"mode"`
	Period string       `json:"period"`
	Rows   []azureRow   `json:"rows"`
	Totals []azureTotal `json:"totals"`
	// Budget is one figure for the whole hub, not one per currency: repeating
	// it beside each total would claim a budget of 100 in euros and another of
	// 100 in dollars. 0 means none was set.
	Budget   float64             `json:"budget"`
	CostAsOf *time.Time          `json:"cost_as_of"`
	Sync     map[string]syncView `json:"sync"`
}

func (s *server) handleGetAzure(w http.ResponseWriter, r *http.Request, _ store.User) {
	cfg := azure.LoadConfig(s.Store)
	period := r.URL.Query().Get("period")
	if period == "" {
		period = s.Now().UTC().Format("2006-01")
	}
	resources, err := s.Store.ListAzureResources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// costByID is one row per resource for this period — the same rows
	// costSummary itself read to compute spent, just keyed for the join
	// below instead of summed across currencies.
	_, _, costByID, costAsOfT, err := costSummary(s.Store, period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var costAsOf *time.Time
	if !costAsOfT.IsZero() {
		costAsOf = &costAsOfT
	}

	rows := make([]azureRow, 0, len(resources)+len(costByID))
	seen := map[string]bool{}
	for _, res := range resources {
		row := azureRow{ID: res.ID, Name: res.Name, Type: res.Type, Group: res.ResourceGroup,
			Location: res.Location, State: res.State, Host: res.Host, Tags: res.Tags,
			Deleted: res.DeletedAt != nil}
		if res.Tags == nil {
			row.Tags = map[string]string{}
		}
		if c, ok := costByID[res.ID]; ok {
			amount := c.Amount
			row.Cost, row.Currency = &amount, c.Currency
		}
		// No cost row means Azure has not reported one. That is not zero, so
		// Cost stays nil and the table shows a dash.
		rows = append(rows, row)
		seen[res.ID] = true
	}
	// A cost row with no inventory row is a resource deleted before this hub
	// ever looked. Showing it is the difference between a right bill and a
	// wrong one. Sorted by id: costByID is a map, and the rows must come out
	// in a stable order across requests.
	goneIDs := make([]string, 0, len(costByID))
	for id := range costByID {
		if !seen[id] {
			goneIDs = append(goneIDs, id)
		}
	}
	sort.Strings(goneIDs)
	for _, id := range goneIDs {
		c := costByID[id]
		amount := c.Amount
		rows = append(rows, azureRow{ID: id, Name: azure.LastSegment(id),
			Type: typeFromID(id), Group: groupFromID(id),
			Cost: &amount, Currency: c.Currency, Deleted: true, Tags: map[string]string{}})
	}

	// One total per currency. Never summed across: adding euros to dollars
	// produces a number that is wrong in a way nobody notices.
	spent := map[string]float64{}
	for _, c := range costByID {
		spent[c.Currency] += c.Amount
	}
	currencies := make([]string, 0, len(spent))
	for cur := range spent {
		currencies = append(currencies, cur)
	}
	sort.Strings(currencies)
	totals := make([]azureTotal, 0, len(currencies))
	for _, cur := range currencies {
		totals = append(totals, azureTotal{Currency: cur, Spent: spent[cur]})
	}

	// Failing loudly rather than returning an empty map: the page reads this to
	// decide what an empty table means, and "I could not read the sync state"
	// must not arrive looking like "no sync has ever run".
	st, err := s.Store.AzureSyncState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	syncs := map[string]syncView{}
	for k, v := range st {
		syncs[k] = syncView{OK: v.OK, Message: v.Message, At: v.EndedAt}
	}
	writeJSON(w, http.StatusOK, azureView{Mode: cfg.Mode, Period: period, Rows: rows,
		Totals: totals, Budget: cfg.BudgetMonthly, CostAsOf: costAsOf, Sync: syncs})
}

// typeFromID returns the type in the casing the id carries, which is the
// lowercase one NormalizeID imposed. Azure's own casing is only in the
// inventory row, and an orphan has none by definition.
func typeFromID(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i, p := range parts {
		if strings.EqualFold(p, "providers") && i+2 < len(parts) {
			return parts[i+1] + "/" + parts[i+2]
		}
	}
	return ""
}

func groupFromID(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourcegroups") {
			return parts[i+1]
		}
	}
	return ""
}

// costSummary is the one place cost rows for a period are read and joined,
// shared by the cost page (handleGetAzure) and the guardrails view
// (handleGetGuardrails): the two must never compute "what was spent" two
// different ways. spent is summed across every currency present — the
// guardrails budget is one figure for the hub (lot 3's decision; see
// guardrails.Budget), unlike the cost page's per-currency totals, which is
// deliberately not this. currencies lists the distinct currency codes seen,
// so a caller that wants to warn about mixing them can. byID is one row per
// resource for the period, for a join; asOf is the latest AsOf across every
// row, or the zero time when there is nothing for the period.
func costSummary(st *store.Store, period string) (spent float64, currencies []string, byID map[string]store.AzureCost, asOf time.Time, err error) {
	costs, err := st.ListAzureCosts(period)
	if err != nil {
		return 0, nil, nil, time.Time{}, err
	}
	byID = make(map[string]store.AzureCost, len(costs))
	currencies = make([]string, 0)
	seen := map[string]bool{}
	for _, c := range costs {
		spent += c.Amount
		byID[c.ResourceID] = c
		if !seen[c.Currency] {
			seen[c.Currency] = true
			currencies = append(currencies, c.Currency)
		}
		if c.AsOf.After(asOf) {
			asOf = c.AsOf
		}
	}
	sort.Strings(currencies)
	return spent, currencies, byID, asOf, nil
}

// azureSettingsView is what the browser sees. The secret is a bool: it goes in,
// it never comes back.
type azureSettingsView struct {
	Mode              string   `json:"mode"`
	TenantID          string   `json:"tenant_id"`
	ClientID          string   `json:"client_id"`
	ClientSecretSet   bool     `json:"client_secret_set"`
	MIClientID        string   `json:"mi_client_id"`
	SubscriptionID    string   `json:"subscription_id"`
	ResourceGroups    []string `json:"resource_groups"`
	InventoryEveryMin int      `json:"inventory_every_min"`
	CostEveryMin      int      `json:"cost_every_min"`
	BudgetMonthly     float64  `json:"budget_monthly"`
	// Provisioning. Saved and read back with the rest, but never validated
	// with it: a hub used only for lot 3's read-only inventory must still be
	// able to save its credentials with all of these empty.
	ProvisionSubnetID  string `json:"provision_subnet_id"`
	ProvisionHubURL    string `json:"provision_hub_url"`
	ProvisionSize      string `json:"provision_size"`
	ProvisionImage     string `json:"provision_image"`
	ProvisionAdminUser string `json:"provision_admin_user"`
	ProvisionSSHKey    string `json:"provision_ssh_key"`
}

// azureSettingsInput mirrors it for writes. ClientSecret is a pointer so that
// "absent" and "empty" stay different answers.
type azureSettingsInput struct {
	Mode              string   `json:"mode"`
	TenantID          string   `json:"tenant_id"`
	ClientID          string   `json:"client_id"`
	ClientSecret      *string  `json:"client_secret"`
	MIClientID        string   `json:"mi_client_id"`
	SubscriptionID    string   `json:"subscription_id"`
	ResourceGroups    []string `json:"resource_groups"`
	InventoryEveryMin int      `json:"inventory_every_min"`
	CostEveryMin      int      `json:"cost_every_min"`
	BudgetMonthly     float64  `json:"budget_monthly"`

	ProvisionSubnetID  string `json:"provision_subnet_id"`
	ProvisionHubURL    string `json:"provision_hub_url"`
	ProvisionSize      string `json:"provision_size"`
	ProvisionImage     string `json:"provision_image"`
	ProvisionAdminUser string `json:"provision_admin_user"`
	ProvisionSSHKey    string `json:"provision_ssh_key"`
}

func (s *server) handleGetAzureSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	c := azure.LoadConfig(s.Store)
	if c.ResourceGroups == nil {
		c.ResourceGroups = []string{}
	}
	pc := azure.LoadProvisionConfig(s.Store)
	writeJSON(w, http.StatusOK, azureSettingsView{
		Mode: c.Mode, TenantID: c.TenantID, ClientID: c.ClientID,
		ClientSecretSet: c.ClientSecret != "", MIClientID: c.MIClientID,
		SubscriptionID: c.SubscriptionID, ResourceGroups: c.ResourceGroups,
		InventoryEveryMin: c.InventoryEveryMin, CostEveryMin: c.CostEveryMin,
		BudgetMonthly: c.BudgetMonthly,
		// The SSH key is a public key, so unlike the client secret it goes
		// back out: there is nothing to protect, and hiding it would make it
		// impossible to check which key is in force.
		ProvisionSubnetID: pc.SubnetID, ProvisionHubURL: pc.HubURL, ProvisionSize: pc.Size,
		ProvisionImage: pc.Image, ProvisionAdminUser: pc.AdminUser, ProvisionSSHKey: pc.SSHKey,
	})
}

func (s *server) handlePutAzureSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in azureSettingsInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	current := azure.LoadConfig(s.Store)
	secret := current.ClientSecret
	if in.ClientSecret != nil { // present in the JSON: the user meant it
		secret = *in.ClientSecret
	}
	// An interval the form left out keeps the one in force, rather than being
	// written as 0 and read back as the default on the next boot.
	if in.InventoryEveryMin <= 0 {
		in.InventoryEveryMin = current.InventoryEveryMin
	}
	if in.CostEveryMin <= 0 {
		in.CostEveryMin = current.CostEveryMin
	}
	c := azure.Config{Mode: in.Mode, TenantID: in.TenantID, ClientID: in.ClientID,
		ClientSecret: secret, MIClientID: in.MIClientID, SubscriptionID: in.SubscriptionID,
		ResourceGroups: in.ResourceGroups, InventoryEveryMin: in.InventoryEveryMin,
		CostEveryMin: in.CostEveryMin, BudgetMonthly: in.BudgetMonthly}
	// Validate before writing: a partial save would leave the hub reading a
	// scope the user never asked for.
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := azure.SaveConfig(s.Store, c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Deliberately not run through Ready(): provisioning is refused when it is
	// asked for, not when the credentials are saved. Validating here would
	// stop a read-only user from saving anything at all.
	if err := azure.SaveProvisionConfig(s.Store, azure.ProvisionConfig{
		SubnetID: in.ProvisionSubnetID, HubURL: in.ProvisionHubURL, Size: in.ProvisionSize,
		Image: in.ProvisionImage, AdminUser: in.ProvisionAdminUser, SSHKey: in.ProvisionSSHKey,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Azure.ReloadAzure()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleTestAzure(w http.ResponseWriter, r *http.Request, _ store.User) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.Azure.TestAzure(ctx); err != nil {
		// 502 with Azure's own words: a test button that says only "failed" is
		// a test button nobody can act on.
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type azureActionInput struct {
	ResourceID string `json:"resource_id"`
	Action     string `json:"action"`
}

// handleStartAzureAction accepts an action and answers immediately. The
// resource id travels in the body rather than the path: an ARM id is mostly
// slashes, a ServeMux wildcard does not match one, and percent-encoding it
// would make the route depend on when net/http unescapes the path.
func (s *server) handleStartAzureAction(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in azureActionInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Vet the verb here: an action the hub does not know must not travel any
	// further towards ARM.
	a := azure.Action(in.Action)
	switch a {
	case azure.ActionStart, azure.ActionStop, azure.ActionRestart:
	default:
		writeErr(w, http.StatusBadRequest, "unknown action")
		return
	}
	if strings.TrimSpace(in.ResourceID) == "" {
		writeErr(w, http.StatusBadRequest, "resource_id is required")
		return
	}

	id, err := s.Azure.StartAction(in.ResourceID, a)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"action_id": id})
	case errors.Is(err, store.ErrNoSuchResource):
		// Outside the configured scope, or deleted. Same answer either way: the
		// hub does not act on a resource it cannot show.
		writeErr(w, http.StatusNotFound, "no such resource")
	case errors.Is(err, store.ErrActionInFlight):
		writeErr(w, http.StatusConflict, "an action is already running on this resource")
	case errors.Is(err, azure.ErrNotConfigured):
		writeErr(w, http.StatusBadRequest, "Azure is not configured")
	case errors.Is(err, azure.ErrNotActionable):
		writeErr(w, http.StatusBadRequest, "this resource type has no actions")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

type azureActionView struct {
	ID           int64   `json:"id"`
	ResourceID   string  `json:"resource_id"`
	ResourceName string  `json:"resource_name"`
	Action       string  `json:"action"`
	Status       string  `json:"status"`
	RequestedAt  string  `json:"requested_at"`
	FinishedAt   *string `json:"finished_at"`
	Error        string  `json:"error"`
	StateBefore  *string `json:"state_before"`
	StateAfter   *string `json:"state_after"`
	// Origin is "user" or "schedule": the recent-actions table shows which
	// boundary or which person issued it.
	Origin string `json:"origin"`
}

func (s *server) handleAzureActions(w http.ResponseWriter, r *http.Request, _ store.User) {
	as, err := s.Store.ListAzureActions(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]azureActionView, 0, len(as))
	for _, a := range as {
		v := azureActionView{ID: a.ID, ResourceID: a.ResourceID, ResourceName: a.ResourceName,
			Action: a.Action, Status: a.Status, RequestedAt: a.RequestedAt.Format(time.RFC3339),
			Error: a.Error, StateBefore: a.StateBefore, StateAfter: a.StateAfter, Origin: a.Origin}
		if a.FinishedAt != nil {
			f := a.FinishedAt.Format(time.RFC3339)
			v.FinishedAt = &f
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out})
}

// --- lot 5: provisioning --------------------------------------------------

type vmCreateInput struct {
	Name string `json:"name"`
}

type vmDeleteInput struct {
	ProvisionID int64  `json:"provision_id"`
	ConfirmName string `json:"confirm_name"`
}

type provisionResourceView struct {
	ARMID     string  `json:"arm_id"`
	Kind      string  `json:"kind"`
	CreatedAt string  `json:"created_at"`
	DeletedAt *string `json:"deleted_at"`
}

type provisionView struct {
	ID          int64                   `json:"id"`
	Name        string                  `json:"name"`
	HostID      *int64                  `json:"host_id"`
	Status      string                  `json:"status"`
	RequestedAt string                  `json:"requested_at"`
	FinishedAt  *string                 `json:"finished_at"`
	Error       string                  `json:"error"`
	DeleteError string                  `json:"delete_error"`
	Resources   []provisionResourceView `json:"resources"`
}

// handleStartProvision answers as soon as the row exists. Creating a VM is
// minutes of work; an HTTP request held open that long dies in a proxy and
// leaves the browser believing a success failed.
func (s *server) handleStartProvision(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in vmCreateInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	id, err := s.Azure.StartProvision(in.Name)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"provision_id": id})
	case errors.Is(err, store.ErrProvisionInFlight):
		writeErr(w, http.StatusConflict, "a VM with this name is already being created")
	case errors.Is(err, azure.ErrNotConfigured):
		writeErr(w, http.StatusBadRequest, "Azure is not configured")
	case azure.IsRefusal(err):
		// A refusal the person can act on — a missing subnet, a hub address no
		// VM could reach, a name Azure would not accept, a subnet outside the
		// watched groups. The message names what is wrong, so it is passed
		// through rather than flattened.
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		// Anything else is the hub's problem, not the caller's. A locked
		// database answering 400 would read as "you typed something wrong".
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *server) handleProvisions(w http.ResponseWriter, r *http.Request, _ store.User) {
	ps, err := s.Store.ListAzureProvisions(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]provisionView, 0, len(ps))
	for _, p := range ps {
		v := provisionView{ID: p.ID, Name: p.Name, HostID: p.HostID, Status: p.Status,
			RequestedAt: p.RequestedAt.Format(time.RFC3339), Error: p.Error,
			DeleteError: p.DeleteError, Resources: make([]provisionResourceView, 0, len(p.Resources))}
		if p.FinishedAt != nil {
			f := p.FinishedAt.Format(time.RFC3339)
			v.FinishedAt = &f
		}
		for _, r := range p.Resources {
			rv := provisionResourceView{ARMID: r.ARMID, Kind: r.Kind,
				CreatedAt: r.CreatedAt.Format(time.RFC3339)}
			if r.DeletedAt != nil {
				d := r.DeletedAt.Format(time.RFC3339)
				rv.DeletedAt = &d
			}
			v.Resources = append(v.Resources, rv)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"provisions": out})
}

func (s *server) handleDeleteProvision(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in vmDeleteInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	err := s.Azure.DeleteProvision(in.ProvisionID, in.ConfirmName)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	case errors.Is(err, store.ErrNoSuchProvision):
		writeErr(w, http.StatusNotFound, "no such provision")
	case errors.Is(err, azure.ErrNotConfigured):
		writeErr(w, http.StatusBadRequest, "Azure is not configured")
	case azure.IsRefusal(err):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
