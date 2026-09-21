package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Deletable is one resource a provision created.
type Deletable struct {
	ARMID string
	Kind  string // nic | vm
}

// ErrNotOurs refuses a resource this hub did not create. It is the guard that
// makes a mistyped id harmless.
var ErrNotOurs = fmt.Errorf("azure: refusing to delete a resource that is not tagged %s=%s",
	CreatedByTag, CreatedByValue)

// deleteOrder is not a preference. Azure refuses to delete a NIC that is still
// attached to a VM, so the VM goes first. The OS disk does not appear because
// it is created with deleteOption "Delete" and goes with the VM — see
// CreateVM.
var deleteOrder = map[string]int{"vm": 0, "nic": 1}

// DeleteCreated removes resources this hub created, in the order Azure
// accepts. onDeleted is called as each one goes, so an interruption halfway
// leaves an accurate record rather than an optimistic one.
//
// Before every DELETE it reads the resource and checks its tags. A read that
// fails is a refusal, not a shrug: not knowing whether a resource is ours is
// not permission to delete it.
func DeleteCreated(ctx context.Context, c *Client, rs []Deletable, onDeleted func(Deletable) error) error {
	ordered := append([]Deletable(nil), rs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return deleteOrder[ordered[i].Kind] < deleteOrder[ordered[j].Kind]
	})

	// Every tag is checked before anything at all is deleted. Checking as we
	// go would let a mistyped set delete the VM and then refuse the NIC,
	// which is the worst of both outcomes.
	for _, r := range ordered {
		// A resource already gone comes back as "not there", which is neither
		// ours nor a refusal: the DELETE below turns its 404 into a record
		// that catches up.
		if _, err := ours(ctx, c, r); err != nil {
			return err
		}
	}

	for _, r := range ordered {
		version, err := apiVersionFor(r.Kind)
		if err != nil {
			return err
		}
		resp, err := c.DeleteAsync(ctx, r.ARMID, url.Values{"api-version": {version}})
		if err != nil {
			// Something already gone is deleted, which is the outcome asked
			// for. The record catches up rather than reporting a failure.
			if StatusOf(err) == http.StatusNotFound {
				if err := onDeleted(r); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if err := Await(ctx, c, resp); err != nil {
			return err
		}
		if err := onDeleted(r); err != nil {
			return err
		}
	}
	return nil
}

// ours reports whether the resource carries this hub's tag. It returns
// gone=true for a 404: a resource that no longer exists cannot be deleted
// wrongly, and refusing on its tag would block the rest of the set.
func ours(ctx context.Context, c *Client, r Deletable) (gone bool, err error) {
	version, err := apiVersionFor(r.Kind)
	if err != nil {
		return false, err
	}
	body, err := c.Get(ctx, r.ARMID, url.Values{"api-version": {version}})
	if err != nil {
		if StatusOf(err) == http.StatusNotFound {
			return true, nil
		}
		return false, fmt.Errorf("azure: cannot read %s to check it is ours: %w", r.ARMID, err)
	}
	var v struct {
		Tags map[string]string `json:"tags"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false, fmt.Errorf("azure: %s did not read as an object: %w", r.ARMID, err)
	}
	if v.Tags[CreatedByTag] != CreatedByValue {
		return false, fmt.Errorf("%w: %s", ErrNotOurs, r.ARMID)
	}
	return false, nil
}

func apiVersionFor(kind string) (string, error) {
	switch strings.ToLower(kind) {
	case "vm":
		return computeAPIVersion, nil
	case "nic":
		return networkAPIVersion, nil
	}
	return "", fmt.Errorf("azure: nothing known about a resource of kind %q", kind)
}
