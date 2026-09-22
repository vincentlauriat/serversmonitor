package azure

import (
	"strings"
	"testing"
)

// settings is the smallest thing that satisfies Getter and Setter.
type settings map[string]string

func (s settings) GetSetting(k string) (string, bool, error) { v, ok := s[k]; return v, ok, nil }
func (s settings) SetSetting(k, v string) error              { s[k] = v; return nil }

const goodSubnet = "/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/" +
	"Microsoft.Network/virtualNetworks/vnet-sandbox/subnets/default"

func readyConfig() ProvisionConfig {
	return ProvisionConfig{
		SubnetID:  goodSubnet,
		HubURL:    "https://monitor.example.net",
		Size:      "Standard_B1s",
		Image:     "Canonical:ubuntu-24_04-lts:server:latest",
		AdminUser: "azureuser",
		SSHKey:    "ssh-ed25519 AAAAC3Nza… vincent@mac",
	}
}

func TestAMissingSubnetIsNamed(t *testing.T) {
	c := readyConfig()
	c.SubnetID = ""
	err := c.Ready()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "subnet") {
		t.Fatalf("the message must name the setting: %v", err)
	}
}

func TestEverythingMissingIsNamedAtOnce(t *testing.T) {
	// One round trip, not five. Somebody configuring this for the first time
	// has all of them empty.
	err := ProvisionConfig{}.Ready()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"subnet", "hub address", "SSH", "administrator"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the message must name %q: %v", want, err)
		}
	}
}

func TestALoopbackHubAddressIsRefusedWithTheReason(t *testing.T) {
	// The failure this catches would otherwise surface twenty minutes later as
	// a VM that booted, ran cloud-init and never dialled in — with nothing in
	// any log on this side, because the agent never reached the hub.
	for _, u := range []string{
		"http://localhost:8091", "http://127.0.0.1:8091", "http://[::1]:8091", "https://LOCALHOST/",
	} {
		c := readyConfig()
		c.HubURL = u
		err := c.Ready()
		if err == nil {
			t.Fatalf("%s was accepted", u)
		}
		if !strings.Contains(err.Error(), "reach") {
			t.Fatalf("%s: the message must say the VM cannot reach it: %v", u, err)
		}
	}
}

func TestAHubAddressMustBeAnAbsoluteHTTPURL(t *testing.T) {
	for _, u := range []string{"monitor.example.net", "ftp://monitor.example.net", "/api"} {
		c := readyConfig()
		c.HubURL = u
		if err := c.Ready(); err == nil {
			t.Fatalf("%s was accepted", u)
		}
	}
}

func TestASubnetIdMustLookLikeASubnetId(t *testing.T) {
	c := readyConfig()
	c.SubnetID = "/subscriptions/SUB/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet"
	err := c.Ready()
	if err == nil {
		t.Fatal("a VNet id is not a subnet id")
	}
	if !strings.Contains(err.Error(), "subnets/") {
		t.Fatalf("the message must show the shape expected: %v", err)
	}
}

func TestParseSubnetIDKeepsARMsCasing(t *testing.T) {
	p, err := ParseSubnetID(goodSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subscription != "SUB" || p.ResourceGroup != "rg-dev-vincent-sandbox" ||
		p.VNet != "vnet-sandbox" || p.Subnet != "default" {
		t.Fatalf("parsed = %+v", p)
	}
	// The VNet id is what the location read addresses, and it must be ARM's
	// own casing: a lowercased id is a bet on ARM being case-insensitive.
	if want := "/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/" +
		"Microsoft.Network/virtualNetworks/vnet-sandbox"; p.VNetID != want {
		t.Fatalf("VNetID = %s\nwant %s", p.VNetID, want)
	}
}

func TestDefaultsApplyAndSavedValuesWin(t *testing.T) {
	s := settings{}
	c := LoadProvisionConfig(s)
	if c.Size != "Standard_B2ats_v2" {
		t.Fatalf("size = %q, want the default", c.Size)
	}
	if !strings.Contains(c.Image, "ubuntu") || c.AdminUser != "azureuser" {
		t.Fatalf("defaults = %+v", c)
	}
	// And nothing is invented for the two that have no sensible default.
	if c.SubnetID != "" || c.HubURL != "" {
		t.Fatalf("a subnet or hub url was invented: %+v", c)
	}

	want := readyConfig()
	want.Size = "Standard_B2s"
	if err := SaveProvisionConfig(s, want); err != nil {
		t.Fatal(err)
	}
	if got := LoadProvisionConfig(s); got != want {
		t.Fatalf("round trip = %+v\nwant %+v", got, want)
	}
}

func TestABlankSizeFallsBackToTheDefault(t *testing.T) {
	// A form that posts an empty size means "I did not choose one". Storing
	// "" and reading it back as "" would turn the default off for good, and
	// the VM PUT would then ask Azure for a machine with no size.
	s := settings{}
	if err := SaveProvisionConfig(s, ProvisionConfig{SubnetID: goodSubnet}); err != nil {
		t.Fatal(err)
	}
	c := LoadProvisionConfig(s)
	if c.Size != defaultVMSize || c.Image != defaultVMImage || c.AdminUser != defaultAdminUser {
		t.Fatalf("blanks were kept: %+v", c)
	}
	// The two with no sensible default stay empty, and Ready says so.
	if c.HubURL != "" {
		t.Fatalf("a hub address was invented: %q", c.HubURL)
	}
}

func TestReadyAcceptsAWholeConfiguration(t *testing.T) {
	if err := readyConfig().Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
}
