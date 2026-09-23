package server

import (
	"context"
	"net/http"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// Notifier is the hub seen from the server: reload the channels after a save,
// and deliver a test message on demand. An interface rather than the hub
// itself, so the server does not import the package that imports it.
type Notifier interface {
	ReloadNotify()
	TestNotify(ctx context.Context, channel string) error
}

// notifyView is what the browser sees. The password is represented by a bool:
// it goes in, it never comes back.
type notifyView struct {
	PublicURL string `json:"public_url"`

	SMTPEnabled     bool     `json:"smtp_enabled"`
	SMTPHost        string   `json:"smtp_host"`
	SMTPPort        int      `json:"smtp_port"`
	SMTPUsername    string   `json:"smtp_username"`
	SMTPPasswordSet bool     `json:"smtp_password_set"`
	SMTPFrom        string   `json:"smtp_from"`
	SMTPTo          []string `json:"smtp_to"`
	SMTPTLS         string   `json:"smtp_tls"`

	WebhookEnabled bool              `json:"webhook_enabled"`
	WebhookURL     string            `json:"webhook_url"`
	WebhookHeaders map[string]string `json:"webhook_headers"`

	TeamsEnabled bool   `json:"teams_enabled"`
	TeamsURL     string `json:"teams_url"`

	Health map[string]healthView `json:"health"`
}

type healthView struct {
	State     string    `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	At        time.Time `json:"at"`
}

// notifyInput mirrors notifyView for writes. Password is a pointer so that
// "absent" and "empty" stay different answers: the browser never receives the
// password and so cannot send it back, but an empty string means "clear it".
type notifyInput struct {
	PublicURL string `json:"public_url"`

	SMTPEnabled  bool     `json:"smtp_enabled"`
	SMTPHost     string   `json:"smtp_host"`
	SMTPPort     int      `json:"smtp_port"`
	SMTPUsername string   `json:"smtp_username"`
	SMTPPassword *string  `json:"smtp_password"`
	SMTPFrom     string   `json:"smtp_from"`
	SMTPTo       []string `json:"smtp_to"`
	SMTPTLS      string   `json:"smtp_tls"`

	WebhookEnabled bool              `json:"webhook_enabled"`
	WebhookURL     string            `json:"webhook_url"`
	WebhookHeaders map[string]string `json:"webhook_headers"`

	TeamsEnabled bool   `json:"teams_enabled"`
	TeamsURL     string `json:"teams_url"`
}

func (s *server) handleGetNotifications(w http.ResponseWriter, r *http.Request, _ store.User) {
	c := notify.LoadConfig(s.Store)
	health := map[string]healthView{}
	if hs, err := s.Store.ChannelHealth(); err == nil {
		for name, d := range hs {
			health[name] = healthView{State: d.State, LastError: d.LastError, At: d.UpdatedAt}
		}
	}
	if c.Webhook.Headers == nil {
		c.Webhook.Headers = map[string]string{}
	}
	if c.SMTP.To == nil {
		c.SMTP.To = []string{}
	}
	writeJSON(w, http.StatusOK, notifyView{
		PublicURL:       c.Public,
		SMTPEnabled:     c.SMTP.Enabled,
		SMTPHost:        c.SMTP.Host,
		SMTPPort:        c.SMTP.Port,
		SMTPUsername:    c.SMTP.Username,
		SMTPPasswordSet: c.SMTP.Password != "",
		SMTPFrom:        c.SMTP.From,
		SMTPTo:          c.SMTP.To,
		SMTPTLS:         c.SMTP.TLSMode,
		WebhookEnabled:  c.Webhook.Enabled,
		WebhookURL:      c.Webhook.URL,
		WebhookHeaders:  c.Webhook.Headers,
		TeamsEnabled:    c.Teams.Enabled,
		TeamsURL:        c.Teams.URL,
		Health:          health,
	})
}

func (s *server) handlePutNotifications(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in notifyInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	current := notify.LoadConfig(s.Store)
	password := current.SMTP.Password
	if in.SMTPPassword != nil { // present in the JSON: the user meant it
		password = *in.SMTPPassword
	}
	if in.WebhookHeaders == nil {
		in.WebhookHeaders = map[string]string{}
	}
	if in.SMTPTLS == "" {
		in.SMTPTLS = "starttls"
	}
	if in.SMTPPort == 0 {
		in.SMTPPort = 587
	}
	c := notify.Config{
		Public: in.PublicURL,
		SMTP: notify.SMTPConfig{Enabled: in.SMTPEnabled, Host: in.SMTPHost, Port: in.SMTPPort,
			Username: in.SMTPUsername, Password: password, From: in.SMTPFrom, To: in.SMTPTo, TLSMode: in.SMTPTLS},
		Webhook: notify.WebhookConfig{Enabled: in.WebhookEnabled, URL: in.WebhookURL, Headers: in.WebhookHeaders},
		Teams:   notify.TeamsConfig{Enabled: in.TeamsEnabled, URL: in.TeamsURL},
	}
	// Validate before writing: a partial save would leave the hub in a state the
	// user never asked for.
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := notify.SaveConfig(s.Store, c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Notify.ReloadNotify()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleTestNotification(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body struct {
		Channel string `json:"channel"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	switch body.Channel {
	case "smtp", "webhook", "teams":
	default:
		writeErr(w, http.StatusBadRequest, "unknown channel")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Notify.TestNotify(ctx, body.Channel); err != nil {
		// 502, with the channel's own words: a test button that says only
		// "failed" is a test button nobody can act on.
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deliveryView struct {
	ID        int64     `json:"id"`
	Channel   string    `json:"channel"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	Host      string    `json:"host"`
	Metric    string    `json:"metric"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
}

func (s *server) handleDeliveries(w http.ResponseWriter, r *http.Request, _ store.User) {
	ds, err := s.Store.ListDeliveries(100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var names map[string]string
	out := make([]deliveryView, 0, len(ds))
	for _, d := range ds {
		v := deliveryView{ID: d.ID, Channel: d.Channel, State: d.State, Attempts: d.Attempts,
			LastError: d.LastError, At: d.UpdatedAt}
		if e, host, err := s.Store.DeliveryEvent(d.ID); err == nil {
			v.Host, v.Metric, v.Kind = host.Name, e.Metric, e.Kind
		} else if ge, err := s.Store.DeliveryGuardrailEvent(d.ID); err == nil {
			if names == nil {
				names = map[string]string{}
				if rs, err := s.Store.ListAzureResources(); err == nil {
					for _, r := range rs {
						names[r.ID] = r.Name
					}
				}
			}
			host := names[ge.Subject]
			if ge.Subject == "budget" {
				host = "Budget"
			} else if host == "" {
				host = azure.LastSegment(ge.Subject)
			}
			v.Host, v.Metric, v.Kind = host, ge.Rule, ge.Kind
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}
