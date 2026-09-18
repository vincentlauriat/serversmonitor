package azure

import (
	"reflect"
	"strings"
	"testing"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(k string) (string, bool, error) { v, ok := f[k]; return v, ok, nil }
func (f fakeSettings) SetSetting(k, v string) error              { f[k] = v; return nil }

func validConfig() Config {
	return Config{Mode: "client_secret", TenantID: "t", ClientID: "c", ClientSecret: "s",
		SubscriptionID: "sub", ResourceGroups: []string{"rg-one", "rg-two"},
		InventoryEveryMin: 15, CostEveryMin: 60, BudgetMonthly: 50}
}

func TestConfigRoundTrip(t *testing.T) {
	st := fakeSettings{}
	want := validConfig()
	if err := SaveConfig(st, want); err != nil {
		t.Fatal(err)
	}
	if got := LoadConfig(st); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadConfigOnAnEmptyStoreIsOff(t *testing.T) {
	c := LoadConfig(fakeSettings{})
	if c.Mode != "off" || c.Enabled() {
		t.Fatalf("a fresh install reads no Azure: %+v", c)
	}
	if c.InventoryEveryMin != 15 || c.CostEveryMin != 60 {
		t.Fatalf("defaults = %d / %d", c.InventoryEveryMin, c.CostEveryMin)
	}
}

func TestLoadConfigSurvivesGarbage(t *testing.T) {
	st := fakeSettings{
		"azure_inventory_interval_min": "not-a-number",
		"azure_budget_monthly":         "beaucoup",
		"azure_resource_groups":        " , rg-one , ,rg-two,",
		"azure_mode":                   "device_code",
	}
	c := LoadConfig(st)
	if c.InventoryEveryMin != 15 {
		t.Fatalf("interval = %d, want the default", c.InventoryEveryMin)
	}
	if c.BudgetMonthly != 0 {
		t.Fatalf("budget = %v, want 0", c.BudgetMonthly)
	}
	if !reflect.DeepEqual(c.ResourceGroups, []string{"rg-one", "rg-two"}) {
		t.Fatalf("groups = %q, blanks must be dropped", c.ResourceGroups)
	}
	if c.Mode != "off" {
		t.Fatalf("a mode nobody implements reads as off, got %q", c.Mode)
	}
}

func TestValidateRejectsWhatWouldFailAtTheNextSync(t *testing.T) {
	cases := map[string]func(*Config){
		"client secret mode with no tenant": func(c *Config) { c.TenantID = "" },
		"client secret mode with no client": func(c *Config) { c.ClientID = "" },
		"client secret mode with no secret": func(c *Config) { c.ClientSecret = "" },
		"no subscription":                   func(c *Config) { c.SubscriptionID = "" },
		"no resource group":                 func(c *Config) { c.ResourceGroups = nil },
		"an interval of zero":               func(c *Config) { c.InventoryEveryMin = 0 },
		"a negative budget":                 func(c *Config) { c.BudgetMonthly = -1 },
		"a mode nobody implements":          func(c *Config) { c.Mode = "device_code" },
	}
	for name, breakIt := range cases {
		c := validConfig()
		breakIt(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	if err := validConfig().Validate(); err != nil {
		t.Errorf("a valid config was rejected: %v", err)
	}
	if err := (Config{Mode: "off"}).Validate(); err != nil {
		t.Errorf("off is always valid: %v", err)
	}
}

func TestEmptyGroupListIsRejectedWithItsReason(t *testing.T) {
	// The role assignment being asked for is on a resource group. A
	// subscription-wide sweep would answer 403, which is worth saying now
	// rather than at three in the morning.
	c := validConfig()
	c.ResourceGroups = nil
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resource group") {
		t.Fatalf("the reason must name resource groups: %v", err)
	}
}

func TestManagedIdentityModeNeedsNoSecret(t *testing.T) {
	c := Config{Mode: "managed_identity", SubscriptionID: "sub",
		ResourceGroups: []string{"rg"}, InventoryEveryMin: 15, CostEveryMin: 60}
	if err := c.Validate(); err != nil {
		t.Fatalf("managed identity carries no credential of its own: %v", err)
	}
}

func TestNewSourceBuildsWhatTheModeAsksFor(t *testing.T) {
	cs, err := NewSource(validConfig(), nil)
	if err != nil || cs.Name() != "client_secret" {
		t.Fatalf("source = %v err = %v", cs, err)
	}
	mi, err := NewSource(Config{Mode: "managed_identity", SubscriptionID: "s",
		ResourceGroups: []string{"rg"}, InventoryEveryMin: 15, CostEveryMin: 60},
		func(string) (string, bool) { return "", false })
	if err != nil || mi.Name() != "managed_identity" {
		t.Fatalf("source = %v err = %v", mi, err)
	}
	if _, err := NewSource(Config{Mode: "off"}, nil); err == nil {
		t.Fatal("off builds no source")
	}
}

func TestSecretNeverAppearsInAnError(t *testing.T) {
	c := validConfig()
	c.ClientSecret = "super-secret-value"
	c.SubscriptionID = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("the secret leaked into an error message: %v", err)
	}
}
