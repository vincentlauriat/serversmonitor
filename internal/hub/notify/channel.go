package notify

import (
	"context"
	"fmt"
	"net/url"
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
}

// Link builds the host page URL, or "" when no public URL is configured:
// a wrong link is worse than none.
func (c Config) Link(hostID int64) string {
	if c.Public == "" {
		return ""
	}
	return strings.TrimSuffix(c.Public, "/") + "/hosts/" + strconv.FormatInt(hostID, 10)
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
