package azure

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Getter and Setter are the two halves of the settings table this package uses.
// Declared here rather than imported from the notification package: a package
// that reads Azure has no business depending on the one that sends mail.
type Getter interface {
	GetSetting(key string) (string, bool, error)
}

type Setter interface {
	SetSetting(key, value string) error
}

type Config struct {
	Mode              string // off | client_secret | managed_identity
	TenantID          string
	ClientID          string
	ClientSecret      string
	MIClientID        string
	SubscriptionID    string
	ResourceGroups    []string
	InventoryEveryMin int
	CostEveryMin      int
	BudgetMonthly     float64
}

func (c Config) Enabled() bool { return c.Mode == "client_secret" || c.Mode == "managed_identity" }

func str(g Getter, key, def string) string {
	v, ok, err := g.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v
}

func integer(g Getter, key string, def int) int {
	n, err := strconv.Atoi(str(g, key, ""))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func number(g Getter, key string) float64 {
	f, err := strconv.ParseFloat(str(g, key, ""), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadConfig never fails: a hub that cannot parse its own settings still has to
// boot and still has to monitor its servers.
func LoadConfig(g Getter) Config {
	mode := str(g, "azure_mode", "off")
	switch mode {
	case "off", "client_secret", "managed_identity":
	default:
		mode = "off"
	}
	return Config{
		Mode:              mode,
		TenantID:          str(g, "azure_tenant_id", ""),
		ClientID:          str(g, "azure_client_id", ""),
		ClientSecret:      str(g, "azure_client_secret", ""),
		MIClientID:        str(g, "azure_mi_client_id", ""),
		SubscriptionID:    str(g, "azure_subscription_id", ""),
		ResourceGroups:    splitList(str(g, "azure_resource_groups", "")),
		InventoryEveryMin: integer(g, "azure_inventory_interval_min", 15),
		CostEveryMin:      integer(g, "azure_cost_interval_min", 60),
		BudgetMonthly:     number(g, "azure_budget_monthly"),
	}
}

func SaveConfig(s Setter, c Config) error {
	for k, v := range map[string]string{
		"azure_mode":                   c.Mode,
		"azure_tenant_id":              c.TenantID,
		"azure_client_id":              c.ClientID,
		"azure_client_secret":          c.ClientSecret,
		"azure_mi_client_id":           c.MIClientID,
		"azure_subscription_id":        c.SubscriptionID,
		"azure_resource_groups":        strings.Join(c.ResourceGroups, ","),
		"azure_inventory_interval_min": strconv.Itoa(c.InventoryEveryMin),
		"azure_cost_interval_min":      strconv.Itoa(c.CostEveryMin),
		"azure_budget_monthly":         strconv.FormatFloat(c.BudgetMonthly, 'f', -1, 64),
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Validate refuses a configuration at save time rather than at the next sync.
// No message ever contains the secret.
func (c Config) Validate() error {
	switch c.Mode {
	case "off":
		return nil
	case "client_secret":
		if c.TenantID == "" || c.ClientID == "" || c.ClientSecret == "" {
			return errors.New("client secret mode needs a tenant id, a client id and a secret")
		}
	case "managed_identity":
		// Nothing of its own: the platform supplies the credential.
	default:
		return fmt.Errorf("mode must be off, client_secret or managed_identity")
	}
	if c.SubscriptionID == "" {
		return errors.New("a subscription id is required")
	}
	if len(c.ResourceGroups) == 0 {
		return errors.New("at least one resource group is required; reading a whole subscription " +
			"needs a subscription-scope role assignment this hub does not request")
	}
	if c.InventoryEveryMin <= 0 || c.CostEveryMin <= 0 {
		return errors.New("both intervals must be at least one minute")
	}
	if c.BudgetMonthly < 0 {
		return errors.New("a budget cannot be negative")
	}
	return nil
}

// NewSource builds the token source the mode asks for, wrapped in the cache.
func NewSource(c Config, env func(string) (string, bool)) (Source, error) {
	if env == nil {
		env = os.LookupEnv
	}
	switch c.Mode {
	case "client_secret":
		return NewCached(NewClientSecret(c.TenantID, c.ClientID, c.ClientSecret), nil), nil
	case "managed_identity":
		return NewCached(NewManagedIdentity(c.MIClientID, env), nil), nil
	}
	return nil, errors.New("azure is off")
}
