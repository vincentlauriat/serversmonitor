package notify

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Getter and Setter are the two halves of the settings table this package uses.
// Channel configuration lives there rather than in environment variables, so
// that it can be edited from the browser without restarting the hub.
type Getter interface {
	GetSetting(key string) (string, bool, error)
}

type Setter interface {
	SetSetting(key, value string) error
}

func str(g Getter, key, def string) string {
	v, ok, err := g.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v
}

func boolean(g Getter, key string) bool { return str(g, key, "") == "true" }

func integer(g Getter, key string, def int) int {
	n, err := strconv.Atoi(str(g, key, ""))
	if err != nil {
		return def
	}
	return n
}

// LoadConfig never fails: a hub that cannot parse its own settings still has to
// boot and still has to monitor. Bad values fall back to the defaults.
func LoadConfig(g Getter) Config {
	headers := map[string]string{}
	if raw := str(g, "notify_webhook_headers", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &headers); err != nil || headers == nil {
			headers = map[string]string{}
		}
	}
	return Config{
		Public: str(g, "notify_public_url", ""),
		SMTP: SMTPConfig{
			Enabled:  boolean(g, "notify_smtp_enabled"),
			Host:     str(g, "notify_smtp_host", ""),
			Port:     integer(g, "notify_smtp_port", 587),
			Username: str(g, "notify_smtp_username", ""),
			Password: str(g, "notify_smtp_password", ""),
			From:     str(g, "notify_smtp_from", ""),
			To:       splitList(str(g, "notify_smtp_to", "")),
			TLSMode:  str(g, "notify_smtp_tls", "starttls"),
		},
		Webhook: WebhookConfig{
			Enabled: boolean(g, "notify_webhook_enabled"),
			URL:     str(g, "notify_webhook_url", ""),
			Headers: headers,
		},
		Teams: TeamsConfig{
			Enabled: boolean(g, "notify_teams_enabled"),
			URL:     str(g, "notify_teams_url", ""),
		},
	}
}

// splitList drops blanks, so a trailing comma does not become a recipient the
// SMTP server rejects for the whole message.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func SaveConfig(s Setter, c Config) error {
	headers, err := json.Marshal(c.Webhook.Headers)
	if err != nil {
		return err
	}
	for k, v := range map[string]string{
		"notify_public_url":      c.Public,
		"notify_smtp_enabled":    strconv.FormatBool(c.SMTP.Enabled),
		"notify_smtp_host":       c.SMTP.Host,
		"notify_smtp_port":       strconv.Itoa(c.SMTP.Port),
		"notify_smtp_username":   c.SMTP.Username,
		"notify_smtp_password":   c.SMTP.Password,
		"notify_smtp_from":       c.SMTP.From,
		"notify_smtp_to":         strings.Join(c.SMTP.To, ","),
		"notify_smtp_tls":        c.SMTP.TLSMode,
		"notify_webhook_enabled": strconv.FormatBool(c.Webhook.Enabled),
		"notify_webhook_url":     c.Webhook.URL,
		"notify_webhook_headers": string(headers),
		"notify_teams_enabled":   strconv.FormatBool(c.Teams.Enabled),
		"notify_teams_url":       c.Teams.URL,
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}
