package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Outbound is what a subnet's own properties say about reaching the
// internet. A VM the hub creates has no public IP, and cloud-init fetches the
// agent from GitHub before it dials the hub, so without an outbound path the
// machine boots, is billed, and never calls in.
//
// It is read from the VNet the hub already GETs for the region: Virtual
// Machine Contributor grants virtualNetworks/read, and a VNet read carries its
// subnets inline. Fixing a subnet is a Microsoft.Network write the role does
// not have, so the hub reports and never repairs.
type Outbound struct {
	State  string `json:"state"`  // nat | default | legacy | route_table | none | unknown
	Detail string `json:"detail"` // one sentence a person can act on
}

const (
	OutboundNAT        = "nat"
	OutboundDefault    = "default"
	OutboundLegacy     = "legacy"
	OutboundRouteTable = "route_table"
	OutboundNone       = "none"
	OutboundUnknown    = "unknown"
)

// Reaches says whether the subnet is known to reach the internet. A route
// table and an unreadable subnet are not known either way, and are not
// counted as reaching it.
func (o Outbound) Reaches() bool {
	return o.State == OutboundNAT || o.State == OutboundDefault || o.State == OutboundLegacy
}

// SubnetOutbound reads one subnet out of a VNet body. The order matters: a
// NAT Gateway wins over everything, because it is what Azure uses first; a
// route table comes before the default outbound flag, because a route of
// 0.0.0.0/0 to a firewall overrides it and the hub cannot read the firewall.
func SubnetOutbound(vnet []byte, subnetID string) Outbound {
	var v struct {
		Properties struct {
			Subnets []struct {
				ID         string `json:"id"`
				Properties struct {
					DefaultOutboundAccess *bool `json:"defaultOutboundAccess"`
					NatGateway            *struct {
						ID string `json:"id"`
					} `json:"natGateway"`
					RouteTable *struct {
						ID string `json:"id"`
					} `json:"routeTable"`
				} `json:"properties"`
			} `json:"subnets"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(vnet, &v); err != nil {
		return Outbound{OutboundUnknown, "The virtual network could not be read, so outbound access is unknown."}
	}
	for _, s := range v.Properties.Subnets {
		// ARM ids are case-insensitive, and the casing a person pastes is not
		// always the casing ARM answers with.
		if !strings.EqualFold(s.ID, subnetID) {
			continue
		}
		p := s.Properties
		switch {
		case p.NatGateway != nil && p.NatGateway.ID != "":
			return Outbound{OutboundNAT, "A NAT Gateway gives this subnet outbound access."}
		case p.RouteTable != nil && p.RouteTable.ID != "":
			return Outbound{OutboundRouteTable, "This subnet sends traffic through route table " + LastSegment(p.RouteTable.ID) +
				". If it leads to a firewall that allows GitHub and this hub, the agent will call in; the hub cannot tell."}
		case p.DefaultOutboundAccess == nil:
			return Outbound{OutboundLegacy, "This subnet predates the change of 2025-09-30 and keeps implicit outbound access."}
		case *p.DefaultOutboundAccess:
			return Outbound{OutboundDefault, "defaultOutboundAccess is true. It works, but Microsoft has deprecated it; a NAT Gateway is the durable answer."}
		default:
			return Outbound{OutboundNone, "This subnet has no outbound access: defaultOutboundAccess is false and there is no NAT Gateway. " +
				"A VM created here boots and its agent never calls in. Set defaultOutboundAccess to true or add a NAT Gateway."}
		}
	}
	return Outbound{OutboundUnknown, "The subnet is not in its virtual network's list, so outbound access is unknown."}
}

// readVNet is the one GET provisioning makes before creating anything: the
// region, which a subnet does not carry, and the subnet's outbound access.
func readVNet(ctx context.Context, c *Client, parts SubnetParts, subnetID string) (string, Outbound, error) {
	body, err := c.Get(ctx, parts.VNetID, url.Values{"api-version": {networkAPIVersion}})
	if err != nil {
		return "", Outbound{}, fmt.Errorf("azure: cannot read the subnet's virtual network, so the region "+
			"a VM would go in is unknown: %w", err)
	}
	var v struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", Outbound{}, fmt.Errorf("azure: the virtual network read is not an object: %w", err)
	}
	if v.Location == "" {
		return "", Outbound{}, fmt.Errorf("azure: %s came back without a location", parts.VNetID)
	}
	return v.Location, SubnetOutbound(body, subnetID), nil
}

// CheckOutbound reads the configured subnet's outbound access on demand, so
// the page can say it before anyone presses Create VM.
func CheckOutbound(ctx context.Context, c *Client, subnetID string) (Outbound, error) {
	parts, err := ParseSubnetID(subnetID)
	if err != nil {
		return Outbound{}, err
	}
	_, out, err := readVNet(ctx, c, parts, subnetID)
	return out, err
}
