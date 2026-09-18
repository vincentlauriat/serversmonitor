package notify

import (
	"reflect"
	"strings"
	"testing"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(k string) (string, bool, error) { v, ok := f[k]; return v, ok, nil }
func (f fakeSettings) SetSetting(k, v string) error              { f[k] = v; return nil }

func TestConfigRoundTrip(t *testing.T) {
	want := Config{
		Public: "https://hub.example",
		SMTP: SMTPConfig{Enabled: true, Host: "smtp.example", Port: 587, Username: "u", Password: "p",
			From: "hub@example", To: []string{"a@example", "b@example"}, TLSMode: "starttls"},
		Webhook: WebhookConfig{Enabled: true, URL: "https://ntfy.example/sm", Headers: map[string]string{"Authorization": "Bearer t"}},
		Teams:   TeamsConfig{Enabled: true, URL: "https://api.powerautomate.com/x"},
	}
	st := fakeSettings{}
	if err := SaveConfig(st, want); err != nil {
		t.Fatal(err)
	}
	got := LoadConfig(st)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadConfigOnAnEmptyStoreIsAllDisabled(t *testing.T) {
	c := LoadConfig(fakeSettings{})
	if c.SMTP.Enabled || c.Webhook.Enabled || c.Teams.Enabled {
		t.Fatalf("a fresh install notifies nobody: %+v", c)
	}
	if len(Enabled(c)) != 0 {
		t.Fatal("no channel must be built")
	}
	if c.SMTP.TLSMode != "starttls" {
		t.Fatalf("the default must be the safe one, got %q", c.SMTP.TLSMode)
	}
}

func TestLoadConfigSurvivesGarbage(t *testing.T) {
	// Settings are text. A hand-edited database must not crash the hub.
	st := fakeSettings{"notify_smtp_port": "not-a-number", "notify_webhook_headers": "{[", "notify_smtp_to": " , a@example , "}
	c := LoadConfig(st)
	if c.SMTP.Port != 587 {
		t.Fatalf("port = %d, want the 587 default", c.SMTP.Port)
	}
	if c.Webhook.Headers == nil || len(c.Webhook.Headers) != 0 {
		t.Fatalf("headers = %v, want an empty map", c.Webhook.Headers)
	}
	if !reflect.DeepEqual(c.SMTP.To, []string{"a@example"}) {
		t.Fatalf("recipients = %q, blanks must be dropped", c.SMTP.To)
	}
}

func TestValidateRejectsWhatWouldFailAtThreeInTheMorning(t *testing.T) {
	cases := map[string]Config{
		"smtp without recipients": {SMTP: SMTPConfig{Enabled: true, Host: "h", Port: 25, From: "f", TLSMode: "none"}},
		"smtp without host":       {SMTP: SMTPConfig{Enabled: true, Port: 25, From: "f", To: []string{"a"}, TLSMode: "none"}},
		"bad tls mode":            {SMTP: SMTPConfig{Enabled: true, Host: "h", Port: 25, From: "f", To: []string{"a"}, TLSMode: "ssl"}},
		"teams over http":         {Teams: TeamsConfig{Enabled: true, URL: "http://example/x"}},
		"webhook not a url":       {Webhook: WebhookConfig{Enabled: true, URL: "not a url"}},
		"public url not a url":    {Public: "hub.example"},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	ok := Config{Public: "https://hub.example", Teams: TeamsConfig{Enabled: true, URL: "https://api.powerautomate.com/x"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid config was rejected: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("the empty config is valid: %v", err)
	}
}

func TestLinkIsEmptyWithoutAPublicURL(t *testing.T) {
	if got := (Config{}).Link(3); got != "" {
		t.Fatalf("link = %q, want empty: a wrong link is worse than none", got)
	}
	if got := (Config{Public: "https://hub.example/"}).Link(3); got != "https://hub.example/hosts/3" {
		t.Fatalf("link = %q", got)
	}
}

func TestRootIsTheDashboardNotAHostPage(t *testing.T) {
	// The test message belongs to no host. Link(0) would send the reader to
	// /hosts/0, a page that does not exist — caught by hand against a running
	// hub, not by a test, which is why this one now exists.
	c := Config{Public: "https://hub.example/"}
	if got := c.Root(); got != "https://hub.example" {
		t.Fatalf("root = %q", got)
	}
	if strings.Contains(c.Root(), "/hosts/") {
		t.Fatalf("the dashboard link must not point at a host page: %q", c.Root())
	}
	if got := (Config{}).Root(); got != "" {
		t.Fatalf("no public url means no link, got %q", got)
	}
}
