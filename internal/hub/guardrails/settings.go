package guardrails

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
)

type Settings struct {
	Thresholds      []int
	ResourceShare   int
	HubVMSilentDays int
	Timezone        string
}

func DefaultSettings() Settings {
	return Settings{Thresholds: []int{80, 100}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "Europe/Paris"}
}

func LoadSettings(g azure.Getter) Settings {
	d := DefaultSettings()
	get := func(k, def string) string {
		v, ok, err := g.GetSetting(k)
		if err != nil || !ok || v == "" {
			return def
		}
		return v
	}
	c := Settings{Timezone: get("azure_timezone", d.Timezone)}
	c.Thresholds = parseThresholds(get("azure_budget_thresholds", "80,100"))
	if len(c.Thresholds) == 0 {
		c.Thresholds = d.Thresholds
	}
	c.ResourceShare = atoiOr(get("azure_resource_share_pct", ""), d.ResourceShare)
	c.HubVMSilentDays = atoiOr(get("azure_hub_vm_silent_days", ""), d.HubVMSilentDays)
	return c
}

func parseThresholds(s string) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func SaveSettings(s azure.Setter, c Settings) error {
	c.Thresholds = parseThresholds(strings.Join(itoa(c.Thresholds), ","))
	for k, v := range map[string]string{
		"azure_budget_thresholds":  strings.Join(itoa(c.Thresholds), ","),
		"azure_resource_share_pct": strconv.Itoa(c.ResourceShare),
		"azure_hub_vm_silent_days": strconv.Itoa(c.HubVMSilentDays),
		"azure_timezone":           c.Timezone,
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

func itoa(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return out
}

func (c Settings) Validate() error {
	if len(c.Thresholds) == 0 {
		return errors.New("at least one budget threshold is required")
	}
	for _, t := range c.Thresholds {
		if t < 1 || t > 500 {
			return fmt.Errorf("a budget threshold must be between 1 and 500 %%, got %d", t)
		}
	}
	if c.ResourceShare < 1 || c.ResourceShare > 100 {
		return fmt.Errorf("the resource share must be between 1 and 100 %%, got %d", c.ResourceShare)
	}
	if c.HubVMSilentDays < 1 {
		return errors.New("the silent-VM age must be at least one day")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "" {
		return fmt.Errorf("unknown time zone %q; use an IANA name such as Europe/Paris", c.Timezone)
	}
	return nil
}

// Location never returns nil. A zone that vanished after being saved is a
// runtime oddity (the zone database is embedded), and the caller logs it.
func (c Settings) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil || c.Timezone == "" {
		return time.UTC
	}
	return loc
}
