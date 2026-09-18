package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// Cost is one resource's spend over the queried period.
type Cost struct {
	ResourceID string
	Currency   string
	Amount     float64
}

// costQuery is the documented month-to-date query, grouped by resource.
func costQuery() map[string]any {
	return map[string]any{
		"type":      "ActualCost",
		"timeframe": "MonthToDate",
		"dataset": map[string]any{
			"granularity": "None",
			"aggregation": map[string]any{
				"total": map[string]any{"name": "Cost", "function": "Sum"},
			},
			"grouping": []any{
				map[string]any{"type": "Dimension", "name": "ResourceId"},
			},
		},
	}
}

type costResponse struct {
	Properties struct {
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]json.RawMessage `json:"rows"`
	} `json:"properties"`
}

// Costs returns month-to-date spend per resource, for every configured group.
//
// The response is columnar and its column order is not the request's order.
// Verified live: the request lists Cost, then ResourceId, then Currency, and
// the response happened to match only by luck. Columns are therefore located by
// name, and a missing one is an error rather than a guess — inventing a number
// is the one thing a cost feature must never do.
func Costs(ctx context.Context, c *Client, subscription string, groups []string) ([]Cost, error) {
	if len(groups) == 0 {
		return nil, errors.New("azure: at least one resource group is required for a cost query")
	}
	var out []Cost
	for _, g := range groups {
		path := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.CostManagement/query", subscription, g)
		raw, err := c.Post(ctx, path, url.Values{"api-version": {"2023-11-01"}}, costQuery())
		if err != nil {
			return nil, fmt.Errorf("cost query on %s: %w", g, err)
		}
		var r costResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("cost response for %s is not json: %w", g, err)
		}
		idx := map[string]int{}
		for i, col := range r.Properties.Columns {
			idx[col.Name] = i
		}
		iCost, okCost := idx["Cost"]
		iID, okID := idx["ResourceId"]
		iCur, okCur := idx["Currency"]
		if !okCost || !okID || !okCur {
			return nil, fmt.Errorf("cost response for %s is missing a column, got %v", g, idx)
		}
		for _, row := range r.Properties.Rows {
			if len(row) <= iCost || len(row) <= iID || len(row) <= iCur {
				return nil, fmt.Errorf("cost row for %s is shorter than its columns", g)
			}
			var amount float64
			var id, currency string
			if err := json.Unmarshal(row[iCost], &amount); err != nil {
				return nil, fmt.Errorf("cost amount for %s: %w", g, err)
			}
			json.Unmarshal(row[iID], &id)
			json.Unmarshal(row[iCur], &currency)
			out = append(out, Cost{ResourceID: NormalizeID(id), Currency: currency, Amount: amount})
		}
	}
	return out, nil
}
