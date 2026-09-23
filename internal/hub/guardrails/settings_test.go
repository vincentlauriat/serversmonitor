package guardrails

import (
	"strings"
	"testing"
	"time"
)

type mem map[string]string

func (m mem) GetSetting(k string) (string, bool, error) { v, ok := m[k]; return v, ok, nil }
func (m mem) SetSetting(k, v string) error              { m[k] = v; return nil }

func TestDefaultsAndRoundTrip(t *testing.T) {
	m := mem{}
	c := LoadSettings(m)
	if got, want := c, DefaultSettings(); got.ResourceShare != want.ResourceShare || got.Timezone != "Europe/Paris" || len(got.Thresholds) != 2 {
		t.Fatalf("defaults = %+v", got)
	}
	c.Thresholds = []int{100, 50, 50}
	c.ResourceShare = 40
	c.HubVMSilentDays = 7
	c.Timezone = "UTC"
	if err := SaveSettings(m, c); err != nil {
		t.Fatal(err)
	}
	back := LoadSettings(m)
	if strings.Join(itoa(back.Thresholds), ",") != "50,100" || back.ResourceShare != 40 || back.HubVMSilentDays != 7 || back.Timezone != "UTC" {
		t.Fatalf("round trip = %+v", back)
	}
	if m["azure_budget_thresholds"] != "50,100" {
		t.Fatalf("stored as %q", m["azure_budget_thresholds"])
	}
}

func TestValidateRefusesWhatTheSpecRefuses(t *testing.T) {
	cases := map[string]Settings{
		"threshold 0":   {Thresholds: []int{0}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "UTC"},
		"threshold 501": {Thresholds: []int{501}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "UTC"},
		"share 0":       {Thresholds: []int{80}, ResourceShare: 0, HubVMSilentDays: 3, Timezone: "UTC"},
		"share 101":     {Thresholds: []int{80}, ResourceShare: 101, HubVMSilentDays: 3, Timezone: "UTC"},
		"silent 0":      {Thresholds: []int{80}, ResourceShare: 30, HubVMSilentDays: 0, Timezone: "UTC"},
		"zone unknown":  {Thresholds: []int{80}, ResourceShare: 30, HubVMSilentDays: 3, Timezone: "Mars/Olympus"},
	}
	for name, c := range cases {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "zone unknown" && !strings.Contains(err.Error(), "Mars/Olympus") {
			t.Errorf("the message must name the zone: %v", err)
		}
	}
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
}

func TestLocationNeverNil(t *testing.T) {
	if (Settings{Timezone: "Nowhere/Nowhere"}).Location() != time.UTC {
		t.Fatal("unknown zone must fall back to UTC")
	}
	if (Settings{Timezone: "Europe/Paris"}).Location().String() != "Europe/Paris" {
		t.Fatal("known zone")
	}
}
