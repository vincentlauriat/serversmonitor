package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// webAPIVersion is the one lot 3's enrichment pass already uses. Actions and
// the state read-back share it on purpose: they talk to the same provider, and
// two versions drifting apart is a bug waiting for a Tuesday.
const webAPIVersion = "2023-12-01"

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

// Actionable maps a resource type to the ARM verb behind each action. A type
// that is absent is not actionable, and the caller says so rather than guessing
// a URL — an App Service Plan is not a site, and stopping one is not a thing.
//
// Lot 5 adds "microsoft.compute/virtualmachines" here, with "deallocate" for
// stop. Those answer 202 and need the async-operation poll; nothing in this map
// does today.
var Actionable = map[string]map[Action]string{
	"microsoft.web/sites": {
		ActionStart:   "start",
		ActionStop:    "stop",
		ActionRestart: "restart",
	},
}

func Supports(resourceType string, a Action) bool {
	_, ok := Actionable[strings.ToLower(resourceType)][a]
	return ok
}

// Do performs one action. armID keeps ARM's own casing: it is what the request
// path is built from.
func Do(ctx context.Context, c *Client, armID, resourceType string, a Action) error {
	verb, ok := Actionable[strings.ToLower(resourceType)][a]
	if !ok {
		return fmt.Errorf("azure: %s cannot be asked to %s", resourceType, a)
	}
	_, err := c.PostAction(ctx, armID+"/"+verb, url.Values{"api-version": {webAPIVersion}})
	return err
}

// ReadState asks Azure what the resource's state is now. It returns nil when
// Azure does not say — not knowing is not "Stopped" — and an error when the
// read itself failed, which the caller must not confuse with the former.
func ReadState(ctx context.Context, c *Client, armID, resourceType string) (*string, error) {
	if _, ok := Actionable[strings.ToLower(resourceType)]; !ok {
		return nil, fmt.Errorf("azure: no state to read for %s", resourceType)
	}
	body, err := c.Get(ctx, armID, url.Values{"api-version": {webAPIVersion}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Properties struct {
			State string `json:"state"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("azure: state read is not an object: %w", err)
	}
	if r.Properties.State == "" {
		return nil, nil
	}
	s := r.Properties.State
	return &s, nil
}

// retryThrottlingOnly is the action retry policy, and it is deliberately
// narrower than the read path's. A 429 means Azure never looked at the request.
// A 5xx on a POST …/stop may land after the stop happened, and repeating it is
// the same double-action that an interrupted row refuses to replay at startup.
func retryThrottlingOnly(err error) bool {
	return Retryable(err) && StatusOf(err) == http.StatusTooManyRequests
}
