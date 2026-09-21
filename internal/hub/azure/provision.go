package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// networkAPIVersion is Microsoft.Network's, and it is not Compute's. Read back
// from the tenant on 2026-09-21 with `az provider show`.
const networkAPIVersion = "2024-10-01"

// CreatedByTag marks everything this hub creates. The delete path refuses
// anything without it, and lot 6 finds orphans by it.
const (
	CreatedByTag   = "createdBy"
	CreatedByValue = "ServersMonitor"
	HostIDTag      = "smHostId"
)

// CreateRequest is one VM to create. Token is a parameter of the run and is
// never stored: it goes into cloud-init and nowhere else.
type CreateRequest struct {
	Name   string
	HostID int64
	Token  string
}

// Created is one resource that now exists in Azure.
type Created struct {
	ARMID string
	Kind  string // nic | vm
}

// CreateVM issues the NIC PUT then the VM PUT, awaiting each one.
//
// onCreated is called as each resource lands, before the next call is
// attempted, so a crash between two PUTs still leaves the first one named. If
// onCreated fails, the run stops: a hub that cannot write down what it created
// must not create more.
//
// It never deletes anything, including on the way out of a failure. That is
// not an omission — see the design note in §4 of the lot 5 spec. A partial run
// leaves resources behind, they are recorded, and a person decides.
//
// The OS disk is deliberately not a third resource. It is created with
// deleteOption "Delete", so it cannot outlive the VM: there is no ordering to
// get wrong on the way out, and the most expensive possible leftover cannot
// happen. A disk that leaked anyway would still show up in the inventory
// sweep, which lists everything in the resource group.
func CreateVM(ctx context.Context, c *Client, p ProvisionConfig, req CreateRequest, onCreated func(Created) error) error {
	if err := p.Ready(); err != nil {
		return err
	}
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("azure: a VM needs a name")
	}
	parts, err := ParseSubnetID(p.SubnetID)
	if err != nil {
		return err
	}
	image, err := parseImage(p.Image)
	if err != nil {
		return err
	}
	customData, err := CloudInit(p.HubURL, req.Token)
	if err != nil {
		return err
	}

	// A subnet has no location of its own; its VNet does. Reading it is one
	// call, and the alternative — defaulting a region — creates the NIC
	// somewhere the subnet is not, which fails at the VM PUT and leaves a
	// leftover produced by a guess.
	location, err := vnetLocation(ctx, c, parts.VNetID)
	if err != nil {
		return err
	}

	tags := map[string]string{CreatedByTag: CreatedByValue}
	group := "/subscriptions/" + parts.Subscription + "/resourceGroups/" + parts.ResourceGroup

	nicID := group + "/providers/Microsoft.Network/networkInterfaces/" + req.Name + "-nic"
	nicBody := map[string]any{
		"location": location,
		"tags":     tags,
		"properties": map[string]any{
			"ipConfigurations": []any{map[string]any{
				"name": "ipconfig1",
				"properties": map[string]any{
					"subnet":                    map[string]any{"id": p.SubnetID},
					"privateIPAllocationMethod": "Dynamic",
					// No publicIPAddress. Virtual Machine Contributor cannot
					// create one, and the agent dials out, so the machine is
					// exactly as monitorable without it and one thing less is
					// exposed. Nobody can SSH in from outside; that is a
					// property of this design, not an accident of it.
				},
			}},
		},
	}
	if err := put(ctx, c, nicID, networkAPIVersion, nicBody); err != nil {
		return err
	}
	if err := onCreated(Created{ARMID: nicID, Kind: "nic"}); err != nil {
		return err
	}

	vmTags := map[string]string{CreatedByTag: CreatedByValue, HostIDTag: strconv.FormatInt(req.HostID, 10)}
	vmID := group + "/providers/Microsoft.Compute/virtualMachines/" + req.Name
	vmBody := map[string]any{
		"location": location,
		"tags":     vmTags,
		"properties": map[string]any{
			"hardwareProfile": map[string]any{"vmSize": p.Size},
			"storageProfile": map[string]any{
				"imageReference": image,
				"osDisk": map[string]any{
					"createOption": "FromImage",
					"managedDisk":  map[string]any{"storageAccountType": "Standard_LRS"},
					"deleteOption": "Delete",
				},
			},
			"osProfile": map[string]any{
				"computerName":  req.Name,
				"adminUsername": p.AdminUser,
				"customData":    customData,
				"linuxConfiguration": map[string]any{
					// There is no password, and there is no public IP either.
					// The SSH key is how the machine is reached from inside
					// the network, and the only way.
					"disablePasswordAuthentication": true,
					"ssh": map[string]any{"publicKeys": []any{map[string]any{
						"path":    "/home/" + p.AdminUser + "/.ssh/authorized_keys",
						"keyData": p.SSHKey,
					}}},
				},
			},
			"networkProfile": map[string]any{
				"networkInterfaces": []any{map[string]any{"id": nicID}},
			},
		},
	}
	if err := put(ctx, c, vmID, computeAPIVersion, vmBody); err != nil {
		return err
	}
	return onCreated(Created{ARMID: vmID, Kind: "vm"})
}

// put sends one create and waits for it to finish. A PUT that returned on its
// 202 would have the next call reference a NIC Azure has not built yet.
func put(ctx context.Context, c *Client, armID, apiVersion string, body any) error {
	resp, err := c.PutAsync(ctx, armID, url.Values{"api-version": {apiVersion}}, body)
	if err != nil {
		return err
	}
	return Await(ctx, c, resp)
}

func vnetLocation(ctx context.Context, c *Client, vnetID string) (string, error) {
	body, err := c.Get(ctx, vnetID, url.Values{"api-version": {networkAPIVersion}})
	if err != nil {
		return "", fmt.Errorf("azure: cannot read the subnet's virtual network, so the region "+
			"a VM would go in is unknown: %w", err)
	}
	var v struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("azure: the virtual network read is not an object: %w", err)
	}
	if v.Location == "" {
		return "", fmt.Errorf("azure: %s came back without a location", vnetID)
	}
	return v.Location, nil
}

// parseImage splits publisher:offer:sku:version, which is how the Azure CLI
// spells an image and therefore how anyone pasting one will have it.
func parseImage(s string) (map[string]any, error) {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 4 {
		return nil, fmt.Errorf("azure: an image looks like publisher:offer:sku:version, got %q", s)
	}
	for _, v := range p {
		if v == "" {
			return nil, fmt.Errorf("azure: an image looks like publisher:offer:sku:version, got %q", s)
		}
	}
	return map[string]any{"publisher": p[0], "offer": p[1], "sku": p[2], "version": p[3]}, nil
}
