package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The api-version each provider publishes. They are not interchangeable:
// 2023-12-01 is a Microsoft.Web version and Microsoft.Compute does not publish
// it at all, so a VM call sent with it is a 400 that no fake would catch.
// Read back from the tenant on 2026-09-21 with `az provider show`.
const (
	webAPIVersion     = "2023-12-01"
	computeAPIVersion = "2024-11-01"
)

// The two refusals a caller has to tell apart from a real Azure failure. They
// live here because the hub raises them and the HTTP layer maps them, and the
// HTTP layer cannot import the hub.
var (
	ErrNotConfigured = errors.New("azure: not configured")
	ErrNotActionable = errors.New("azure: this resource type has no actions")
)

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

// provider is everything that differs between two resource types: how to
// address them, what each action is called, and how to find a state in what
// they answer. All three used to be package-level constants shaped for
// Microsoft.Web, which worked only as long as Web was the only provider.
type provider struct {
	apiVersion string
	// verbs maps an action to the ARM verb behind it. A type that is absent is
	// not actionable, and the caller says so rather than guessing a URL — an
	// App Service Plan is not a site, and stopping one is not a thing.
	verbs map[Action]string
	// statePath is what the state read-back addresses. A site reads itself; a
	// VM's power state lives in a sub-resource.
	statePath func(armID string) string
	// readState finds the state in that answer, or returns nil when the answer
	// does not carry one. nil is "nobody said", which is never "stopped".
	readState func(body []byte) (*string, error)
}

var providers = map[string]provider{
	"microsoft.web/sites": {
		apiVersion: webAPIVersion,
		verbs: map[Action]string{
			ActionStart:   "start",
			ActionStop:    "stop",
			ActionRestart: "restart",
		},
		statePath: func(armID string) string { return armID },
		readState: readSiteState,
	},
	"microsoft.compute/virtualmachines": {
		apiVersion: computeAPIVersion,
		verbs: map[Action]string{
			ActionStart: "start",
			// Not "stop". "powerOff" keeps the machine allocated and billed;
			// somebody who pressed Stop to save money would keep paying.
			ActionStop:    "deallocate",
			ActionRestart: "restart",
		},
		statePath: func(armID string) string { return armID + "/instanceView" },
		readState: readVMPowerState,
	},
}

// readSiteState reads properties.state, which is how Microsoft.Web answers.
func readSiteState(body []byte) (*string, error) {
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

// readVMPowerState reads the instance view's statuses, keeping only the
// PowerState line. The ProvisioningState line sits right next to it and says
// nothing about whether the machine is on; borrowing it would be exactly the
// kind of plausible guess this project refuses.
//
// The code suffix is returned as Azure spells it ("running", "deallocated").
// Capitalising it here would be this package inventing presentation.
func readVMPowerState(body []byte) (*string, error) {
	var r struct {
		Statuses []struct {
			Code string `json:"code"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("azure: instance view is not an object: %w", err)
	}
	const prefix = "PowerState/"
	for _, st := range r.Statuses {
		if strings.HasPrefix(st.Code, prefix) {
			s := strings.TrimPrefix(st.Code, prefix)
			if s == "" {
				return nil, nil
			}
			return &s, nil
		}
	}
	return nil, nil
}

func providerFor(resourceType string) (provider, bool) {
	p, ok := providers[strings.ToLower(resourceType)]
	return p, ok
}

func Supports(resourceType string, a Action) bool {
	p, ok := providerFor(resourceType)
	if !ok {
		return false
	}
	_, ok = p.verbs[a]
	return ok
}

// Do performs one action. armID keeps ARM's own casing: it is what the request
// path is built from.
func Do(ctx context.Context, c *Client, armID, resourceType string, a Action) error {
	p, ok := providerFor(resourceType)
	if !ok {
		return fmt.Errorf("%w: %s cannot be asked to %s", ErrNotActionable, resourceType, a)
	}
	verb, ok := p.verbs[a]
	if !ok {
		return fmt.Errorf("%w: %s cannot be asked to %s", ErrNotActionable, resourceType, a)
	}
	// Microsoft.Web answers an action synchronously; Microsoft.Compute answers
	// 202 and finishes minutes later. Await handles both, and a hub that
	// returned on the 202 would report a VM stopped while it was still
	// shutting down — and then read a state back that contradicted it.
	resp, err := c.PostActionAsync(ctx, armID+"/"+verb, url.Values{"api-version": {p.apiVersion}})
	if err != nil {
		return err
	}
	return Await(ctx, c, resp)
}

// ReadState asks Azure what the resource's state is now. It returns nil when
// Azure does not say — not knowing is not "Stopped" — and an error when the
// read itself failed, which the caller must not confuse with the former.
func ReadState(ctx context.Context, c *Client, armID, resourceType string) (*string, error) {
	p, ok := providerFor(resourceType)
	if !ok {
		return nil, fmt.Errorf("azure: no state to read for %s", resourceType)
	}
	body, err := c.Get(ctx, p.statePath(armID), url.Values{"api-version": {p.apiVersion}})
	if err != nil {
		return nil, err
	}
	return p.readState(body)
}

// retryThrottlingOnly is the action retry policy, and it is deliberately
// narrower than the read path's. A 429 means Azure never looked at the request.
// A 5xx on a POST …/stop may land after the stop happened, and repeating it is
// the same double-action that an interrupted row refuses to replay at startup.
func retryThrottlingOnly(err error) bool {
	return Retryable(err) && StatusOf(err) == http.StatusTooManyRequests
}
