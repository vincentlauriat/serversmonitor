package notify

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Channel delivers one message. It knows nothing about retries or queues.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

type SMTPConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	TLSMode  string // starttls | tls | none
}

type WebhookConfig struct {
	Enabled bool
	URL     string
	Headers map[string]string
}

type TeamsConfig struct {
	Enabled bool
	URL     string
}

type Config struct {
	Public  string
	SMTP    SMTPConfig
	Webhook WebhookConfig
	Teams   TeamsConfig
	// The routes of the transitions that have no rule row: a host going
	// offline, and the Azure guardrails. nil is every enabled channel, empty is
	// none, the same convention as a rule's Channels.
	OfflineRoute   []string
	GuardrailRoute []string
}

// ChannelNames is every channel the hub knows, in the order it builds them.
var ChannelNames = []string{"smtp", "webhook", "teams"}

// NormalizeRoute checks a route and puts it in channel order without
// duplicates, so one route reads the same in the store, the API and the page.
// nil stays nil: "every channel" is not the list of today's channels, it also
// covers one enabled tomorrow.
func NormalizeRoute(names []string) ([]string, error) {
	if names == nil {
		return nil, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		if !slices.Contains(ChannelNames, n) {
			return nil, fmt.Errorf("unknown channel %q, must be smtp, webhook or teams", n)
		}
		want[n] = true
	}
	out := []string{}
	for _, n := range ChannelNames {
		if want[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// Route keeps the enabled channels a transition is owed: those its route
// names (all of them when the route is nil), plus those named in also. A
// resolved passes the channels its fired went to as also, so a channel that
// heard an alert start hears it end even after the rule was re-routed. A name
// that is not enabled is skipped: nothing can deliver through it.
func Route(chans []Channel, route, also []string) []Channel {
	var out []Channel
	for _, ch := range chans {
		if route == nil || slices.Contains(route, ch.Name()) || slices.Contains(also, ch.Name()) {
			out = append(out, ch)
		}
	}
	return out
}

// Link builds the host page URL, or "" when no public URL is configured:
// a wrong link is worse than none.
func (c Config) Link(hostID int64) string {
	if c.Public == "" {
		return ""
	}
	return strings.TrimSuffix(c.Public, "/") + "/hosts/" + strconv.FormatInt(hostID, 10)
}

// Root is the dashboard URL, or "" when no public URL is configured. Used by
// the test message, which belongs to no host: Link(0) would point at a host
// page that does not exist, and a link that goes somewhere wrong is worse than
// no link at all.
func (c Config) Root() string {
	if c.Public == "" {
		return ""
	}
	return strings.TrimSuffix(c.Public, "/")
}

// AzureLink builds the Azure page URL, or "" when no public URL is
// configured: a wrong link is worse than none, same rule as Link and Root.
func (c Config) AzureLink() string {
	if c.Public == "" {
		return ""
	}
	return c.Root() + "/azure"
}

// Validate refuses a configuration at save time rather than at 3 a.m.
func (c Config) Validate() error {
	if c.Public != "" {
		u, err := url.Parse(c.Public)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("public url must be http(s)://host")
		}
	}
	if c.SMTP.Enabled {
		if c.SMTP.Host == "" || c.SMTP.Port <= 0 || c.SMTP.From == "" || len(c.SMTP.To) == 0 {
			return fmt.Errorf("smtp needs a host, a port, a sender and at least one recipient")
		}
		switch c.SMTP.TLSMode {
		case "starttls", "tls", "none":
		default:
			return fmt.Errorf("smtp tls mode must be starttls, tls or none")
		}
	}
	if err := requireURL("webhook", c.Webhook.Enabled, c.Webhook.URL, false); err != nil {
		return err
	}
	if _, err := NormalizeRoute(c.OfflineRoute); err != nil {
		return fmt.Errorf("offline routing: %w", err)
	}
	if _, err := NormalizeRoute(c.GuardrailRoute); err != nil {
		return fmt.Errorf("guardrail routing: %w", err)
	}
	return requireURL("teams", c.Teams.Enabled, c.Teams.URL, true)
}

func requireURL(name string, enabled bool, raw string, httpsOnly bool) error {
	if !enabled {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s url is not a url", name)
	}
	if httpsOnly && u.Scheme != "https" {
		return fmt.Errorf("%s url must be https", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s url must be http(s)", name)
	}
	return nil
}

// Enabled builds the channels the configuration asks for, in a stable order.
func Enabled(c Config) []Channel {
	var out []Channel
	if c.SMTP.Enabled {
		out = append(out, NewSMTP(c.SMTP))
	}
	if c.Webhook.Enabled {
		out = append(out, NewWebhook(c.Webhook))
	}
	if c.Teams.Enabled {
		out = append(out, NewTeams(c.Teams))
	}
	return out
}
