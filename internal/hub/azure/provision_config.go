package azure

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ProvisionConfig is what creating a VM needs on top of the credentials.
//
// It is deliberately separate from Config, and so is its validation: Config is
// checked when the Azure settings are saved, and putting the subnet or the hub
// address there would stop someone who only wants lot 3's read-only inventory
// from saving anything at all. These are checked when a provision is asked
// for, which is the moment they are actually needed.
type ProvisionConfig struct {
	// SubnetID is the full ARM id of an existing subnet. The hub never creates
	// a network: Virtual Machine Contributor grants subnets/join/action and
	// virtualNetworks/read, and no write anywhere in Microsoft.Network beyond
	// the NIC. Read back from the live role definition on 2026-09-21.
	SubnetID string
	// HubURL is what the agent dials. The hub does not guess its own address:
	// it usually gets it wrong, and it gets it wrong in the one way that is
	// invisible until a VM has booted and failed to call home.
	HubURL    string
	Size      string
	Image     string // publisher:offer:sku:version
	AdminUser string
	SSHKey    string // one authorized_keys line
}

// defaultVMSize is the cheapest burstable size the sandbox's subscription can
// actually get. Standard_B1s was the first choice (7.52 EUR/month against
// 12.56), and it is not offered to this subscription in West Europe at all:
// `az vm list-skus` does not list any B-series v1 size there, and the first
// real provisioning attempt on 2026-09-22 failed with "Capacity Restrictions:
// Standard_B1s". A default that fails on the first try is worse than a
// slightly dearer one that works; the size stays editable in the settings.
const (
	defaultVMSize    = "Standard_B2ats_v2"
	defaultVMImage   = "Canonical:ubuntu-24_04-lts:server:latest"
	defaultAdminUser = "azureuser"
)

func LoadProvisionConfig(g Getter) ProvisionConfig {
	return ProvisionConfig{
		SubnetID: strings.TrimSpace(str(g, "azure_provision_subnet_id", "")),
		HubURL:   strings.TrimSpace(str(g, "azure_provision_hub_url", "")),
		// These three fall back on an empty stored value too, not only on a
		// missing key: a form that posts a blank size means "I did not choose
		// one", and persisting "" would quietly turn the default off for good.
		Size:      orDefault(str(g, "azure_provision_size", ""), defaultVMSize),
		Image:     orDefault(str(g, "azure_provision_image", ""), defaultVMImage),
		AdminUser: orDefault(str(g, "azure_provision_admin_user", ""), defaultAdminUser),
		SSHKey:    strings.TrimSpace(str(g, "azure_provision_ssh_key", "")),
	}
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v == "" {
		return def
	}
	return v
}

func SaveProvisionConfig(s Setter, c ProvisionConfig) error {
	for k, v := range map[string]string{
		"azure_provision_subnet_id":  c.SubnetID,
		"azure_provision_hub_url":    c.HubURL,
		"azure_provision_size":       c.Size,
		"azure_provision_image":      c.Image,
		"azure_provision_admin_user": c.AdminUser,
		"azure_provision_ssh_key":    c.SSHKey,
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Ready refuses a provision and says what is missing — all of it at once,
// because the first time anyone configures this everything is empty and five
// round trips is five round trips.
func (c ProvisionConfig) Ready() error {
	var missing []string
	if c.SubnetID == "" {
		missing = append(missing, "a subnet id (the hub cannot create a network; create a VNet "+
			"with a subnet yourself and paste its id here)")
	} else if _, err := ParseSubnetID(c.SubnetID); err != nil {
		missing = append(missing, err.Error())
	}
	if c.HubURL == "" {
		missing = append(missing, "the hub address the agent should dial")
	} else if err := checkHubURL(c.HubURL); err != nil {
		missing = append(missing, err.Error())
	}
	if c.Size == "" {
		missing = append(missing, "a VM size")
	}
	if c.Image == "" {
		missing = append(missing, "an image")
	}
	if c.AdminUser == "" {
		missing = append(missing, "an administrator user name")
	}
	if c.SSHKey == "" {
		missing = append(missing, "an SSH public key (there is no password login, and no public "+
			"IP either, so this is how you reach the machine from inside the network)")
	}
	if len(missing) == 0 {
		return nil
	}
	return Refuse("provisioning is not configured: %s", strings.Join(missing, "; "))
}

// checkHubURL rejects an address no VM in Azure could resolve. Catching it
// here costs a sentence; catching it in production costs twenty minutes and a
// machine that booted, ran cloud-init, and silently never called home.
func checkHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Refuse("the hub address must be an absolute http(s) URL, got %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return Refuse("a VM in Azure cannot reach %q; give the hub an address the VM resolves", raw)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return Refuse("a VM in Azure cannot reach %q; give the hub an address the VM resolves", raw)
	}
	return nil
}

// SubnetParts is an ARM subnet id taken apart, keeping ARM's own casing
// throughout: every id built from these goes back into a request path.
type SubnetParts struct {
	Subscription  string
	ResourceGroup string
	VNet          string
	Subnet        string
	// VNetID is the parent VNet's id, which is what the location read
	// addresses — a subnet has no location of its own.
	VNetID string
}

func ParseSubnetID(id string) (SubnetParts, error) {
	bad := Refuse("a subnet id looks like /subscriptions/…/resourceGroups/…/providers/"+
		"Microsoft.Network/virtualNetworks/…/subnets/…, got %q", id)
	p := strings.Split(strings.TrimPrefix(strings.TrimSpace(id), "/"), "/")
	if len(p) != 10 ||
		!strings.EqualFold(p[0], "subscriptions") ||
		!strings.EqualFold(p[2], "resourceGroups") ||
		!strings.EqualFold(p[4], "providers") ||
		!strings.EqualFold(p[5], "Microsoft.Network") ||
		!strings.EqualFold(p[6], "virtualNetworks") ||
		!strings.EqualFold(p[8], "subnets") {
		return SubnetParts{}, bad
	}
	for _, s := range []string{p[1], p[3], p[7], p[9]} {
		if s == "" {
			return SubnetParts{}, bad
		}
	}
	return SubnetParts{
		Subscription:  p[1],
		ResourceGroup: p[3],
		VNet:          p[7],
		Subnet:        p[9],
		VNetID:        "/" + strings.Join(p[:8], "/"),
	}, nil
}

// Refusal is something the person asked for that the hub will not do, and that
// they can act on: a setting that is missing, a name Azure would reject, a
// subnet outside the watched groups, a confirmation that does not match.
//
// It exists so the HTTP layer can tell those apart from a failure, which is
// the hub's problem and answers 500. Without it a locked database would render
// as "400 database is locked", which reads as "you typed something wrong".
type Refusal struct{ Err error }

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// Refuse wraps a message the person can act on.
func Refuse(format string, a ...any) error { return &Refusal{Err: fmt.Errorf(format, a...)} }

// IsRefusal reports whether the failure is the caller's to fix.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}
