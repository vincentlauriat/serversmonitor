package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Resource is one Azure resource as ServersMonitor stores it.
// State is a pointer: nil means nobody read it, which is never "stopped".
type Resource struct {
	ID                string
	Name              string
	Type              string
	ResourceGroup     string
	Location          string
	Kind              string
	SKU               string
	State             *string
	ProvisioningState string
	Host              string
	Tags              map[string]string
}

// NormalizeID lowercases an ARM resource id. ARM returns `resourceGroups` and
// `Microsoft.Web`; Cost Management returns `resourcegroups` and `microsoft.web`
// for the same resource. Every id is normalised on the way in so the two join.
func NormalizeID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// resourceGroupOf pulls the group out of an id, which is more reliable than the
// generic list's own field and works for the typed passes too.
func resourceGroupOf(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

type armResource struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Location string            `json:"location"`
	Kind     string            `json:"kind"`
	Tags     map[string]string `json:"tags"`
	SKU      struct {
		Name string `json:"name"`
	} `json:"sku"`
	Properties struct {
		State             string `json:"state"`
		Status            string `json:"status"`
		ProvisioningState string `json:"provisioningState"`
		DefaultHostName   string `json:"defaultHostName"`
	} `json:"properties"`
}

// enrichmentPaths are the typed provider lists this lot understands. A type
// that is not here is still listed, with a nil state: an inventory that
// silently omits what it does not understand is worse than one that admits it.
var enrichmentPaths = []string{
	"/providers/Microsoft.Web/sites",
	"/providers/Microsoft.Web/serverfarms",
}

// Inventory runs the catalogue pass over every configured group, then one typed
// enrichment pass per provider it understands.
//
// It returns an error if any call fails. A partial inventory is worse than
// none: the caller's sweep would read the missing rows as deletions.
func Inventory(ctx context.Context, c *Client, subscription string, groups []string) ([]Resource, error) {
	if len(groups) == 0 {
		return nil, errors.New("azure: at least one resource group is required; " +
			"reading a whole subscription needs a subscription-scope role assignment this hub does not request")
	}
	byID := map[string]*Resource{}
	var order []string

	for _, g := range groups {
		base := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscription, g)
		items, err := c.GetAll(ctx, base+"/resources", url.Values{"api-version": {"2021-04-01"}})
		if err != nil {
			return nil, fmt.Errorf("catalogue pass on %s: %w", g, err)
		}
		for _, raw := range items {
			var a armResource
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("catalogue entry in %s: %w", g, err)
			}
			id := NormalizeID(a.ID)
			r := &Resource{ID: id, Name: a.Name, Type: a.Type, Location: a.Location,
				Kind: a.Kind, SKU: a.SKU.Name, Tags: a.Tags, ResourceGroup: resourceGroupOf(a.ID)}
			if r.ResourceGroup == "" {
				r.ResourceGroup = g
			}
			if r.Tags == nil {
				r.Tags = map[string]string{}
			}
			byID[id] = r
			order = append(order, id)
		}

		for _, path := range enrichmentPaths {
			items, err := c.GetAll(ctx, base+path, url.Values{"api-version": {"2023-12-01"}})
			if err != nil {
				return nil, fmt.Errorf("enrichment pass %s on %s: %w", path, g, err)
			}
			for _, raw := range items {
				var a armResource
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				r, ok := byID[NormalizeID(a.ID)]
				if !ok {
					continue // enriched something the catalogue did not list
				}
				state := a.Properties.State
				if state == "" {
					state = a.Properties.Status
				}
				if state != "" {
					s := state
					r.State = &s
				}
				if a.Properties.ProvisioningState != "" {
					r.ProvisioningState = a.Properties.ProvisioningState
				}
				if a.Properties.DefaultHostName != "" {
					r.Host = a.Properties.DefaultHostName
				}
				if a.SKU.Name != "" {
					r.SKU = a.SKU.Name
				}
				if a.Kind != "" {
					r.Kind = a.Kind
				}
			}
		}
	}

	out := make([]Resource, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}
