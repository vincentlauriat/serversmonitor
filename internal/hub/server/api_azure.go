package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// Azurer is the hub seen from the server: reload the client after a save, and
// run one connection test on demand. An interface rather than the hub itself,
// so the server does not import the package that imports it.
type Azurer interface {
	ReloadAzure()
	TestAzure(ctx context.Context) error
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
	costs, err := s.Store.ListAzureCosts(period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	costByID := map[string]store.AzureCost{}
	var costAsOf *time.Time
	for _, c := range costs {
		costByID[c.ResourceID] = c
		if costAsOf == nil || c.AsOf.After(*costAsOf) {
			t := c.AsOf
			costAsOf = &t
		}
	}

	rows := make([]azureRow, 0, len(resources)+len(costs))
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
	// wrong one.
	for _, c := range costs {
		if seen[c.ResourceID] {
			continue
		}
		amount := c.Amount
		rows = append(rows, azureRow{ID: c.ResourceID, Name: lastSegment(c.ResourceID),
			Type: typeFromID(c.ResourceID), Group: groupFromID(c.ResourceID),
			Cost: &amount, Currency: c.Currency, Deleted: true, Tags: map[string]string{}})
	}

	// One total per currency. Never summed across: adding euros to dollars
	// produces a number that is wrong in a way nobody notices.
	spent := map[string]float64{}
	for _, c := range costs {
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

// lastSegment stands in for a name when a cost row has no inventory row,
// because "/subscriptions/…/components/long-gone" is not a name anyone reads.
func lastSegment(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
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
}

func (s *server) handleGetAzureSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	c := azure.LoadConfig(s.Store)
	if c.ResourceGroups == nil {
		c.ResourceGroups = []string{}
	}
	writeJSON(w, http.StatusOK, azureSettingsView{
		Mode: c.Mode, TenantID: c.TenantID, ClientID: c.ClientID,
		ClientSecretSet: c.ClientSecret != "", MIClientID: c.MIClientID,
		SubscriptionID: c.SubscriptionID, ResourceGroups: c.ResourceGroups,
		InventoryEveryMin: c.InventoryEveryMin, CostEveryMin: c.CostEveryMin,
		BudgetMonthly: c.BudgetMonthly,
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
