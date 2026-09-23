package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	OrphanDiskUnattached  = "disk_unattached"
	OrphanIPUnassociated  = "ip_unassociated"
	OrphanNICWithoutVM    = "nic_without_vm"
	OrphanPlanWithoutSite = "plan_without_site"
	OrphanHubVMSilent     = "hub_vm_silent" // decided by the hub, not here
	OrphanUnverified      = "unverified"
)

const diskAPIVersion = "2024-03-02"

// detector reads one resource and says whether it is an orphan. The body is
// Azure's own JSON; each detector reads the one property that decides.
type detector struct {
	apiVersion string
	reason     string
	orphan     func(body []byte) (bool, error)
}

var detectors = map[string]detector{
	"microsoft.compute/disks": {apiVersion: diskAPIVersion, reason: OrphanDiskUnattached, orphan: func(b []byte) (bool, error) {
		var v struct {
			Properties struct {
				DiskState string `json:"diskState"`
			} `json:"properties"`
		}
		return unmarshalProp(b, &v, func() bool { return v.Properties.DiskState == "Unattached" })
	}},
	"microsoft.network/publicipaddresses": {apiVersion: networkAPIVersion, reason: OrphanIPUnassociated, orphan: func(b []byte) (bool, error) {
		var v struct {
			Properties struct {
				IPConfiguration *struct {
					ID string `json:"id"`
				} `json:"ipConfiguration"`
			} `json:"properties"`
		}
		return unmarshalProp(b, &v, func() bool { return v.Properties.IPConfiguration == nil })
	}},
	"microsoft.network/networkinterfaces": {apiVersion: networkAPIVersion, reason: OrphanNICWithoutVM, orphan: func(b []byte) (bool, error) {
		var v struct {
			Properties struct {
				VirtualMachine *struct {
					ID string `json:"id"`
				} `json:"virtualMachine"`
			} `json:"properties"`
		}
		return unmarshalProp(b, &v, func() bool { return v.Properties.VirtualMachine == nil })
	}},
	"microsoft.web/serverfarms": {apiVersion: webAPIVersion, reason: OrphanPlanWithoutSite, orphan: func(b []byte) (bool, error) {
		var v struct {
			Properties struct {
				NumberOfSites *int `json:"numberOfSites"`
			} `json:"properties"`
		}
		return unmarshalProp(b, &v, func() bool { return v.Properties.NumberOfSites != nil && *v.Properties.NumberOfSites == 0 })
	}},
}

func unmarshalProp(b []byte, v any, decide func() bool) (bool, error) {
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("azure: orphan read did not parse: %w", err)
	}
	return decide(), nil
}

// OrphanReasons decides, per resource of a type it knows, whether it is an
// orphan. A 403 is recorded as unverified rather than silently healthy: the
// hub's role may not cover the read, and "not verified" is the truthful
// answer — an absent value is never zero, silence is never success. A 404 is
// skipped: the resource left between the catalogue pass and this read. Any
// other failure aborts the sweep, as a partial inventory would.
func OrphanReasons(ctx context.Context, c *Client, rs []Resource) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range rs {
		d, ok := detectors[strings.ToLower(r.Type)]
		if !ok {
			continue
		}
		body, err := c.Get(ctx, r.ARMID, url.Values{"api-version": {d.apiVersion}})
		switch StatusOf(err) {
		case 0:
			// nil error (fall through to parsing below) or a transport error
			// with no HTTP status (caught by the err != nil check below).
		case http.StatusForbidden:
			out[r.ID] = OrphanUnverified
			continue
		case http.StatusNotFound:
			continue
		default:
			return nil, fmt.Errorf("orphan read on %s: %w", r.Name, err)
		}
		if err != nil { // a transport error has no status
			return nil, fmt.Errorf("orphan read on %s: %w", r.Name, err)
		}
		orphan, err := d.orphan(body)
		if err != nil {
			return nil, err
		}
		if orphan {
			out[r.ID] = d.reason
		}
	}
	return out, nil
}
