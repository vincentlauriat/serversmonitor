package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// WebhookPayload is the contract this hub publishes. ntfy reads message, title,
// priority and tags; the rest is there for a receiver that wants to route on
// the host or the metric.
type WebhookPayload struct {
	Host      string   `json:"host"`
	HostID    int64    `json:"host_id"`
	Metric    string   `json:"metric"`
	Kind      string   `json:"kind"`
	Value     float64  `json:"value"`
	Threshold float64  `json:"threshold"`
	Title     string   `json:"title"`
	Message   string   `json:"message"`
	Priority  int      `json:"priority"`
	Tags      []string `json:"tags"`
	Link      string   `json:"link,omitempty"`
	At        string   `json:"at"`
}

type Webhook struct {
	cfg    WebhookConfig
	client *http.Client
}

func NewWebhook(cfg WebhookConfig) *Webhook {
	return &Webhook{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(WebhookPayload{
		Host: m.HostName, HostID: m.HostID, Metric: m.Metric, Kind: m.Kind,
		Value: m.Value, Threshold: m.Threshold, Title: m.Title(), Message: m.Body(),
		Priority: m.Priority(), Tags: m.Tags(), Link: m.Link,
		At: m.At.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err // a payload we cannot even encode will never encode
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ServersMonitor")
	// ntfy takes these from headers even when the body is JSON.
	req.Header.Set("X-Title", sanitizeHeader(m.Title()))
	req.Header.Set("X-Priority", strconv.Itoa(m.Priority()))
	for k, v := range w.cfg.Headers {
		req.Header.Set(k, sanitizeHeader(v))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return err // shutting down, not a channel failure
		}
		return MarkRetryable(fmt.Errorf("webhook post: %w", err))
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classifyHTTP(resp.StatusCode, detail)
}

// classifyHTTP is shared with Teams: 2xx is done, 429 and 5xx are worth another
// attempt, every other 4xx is a configuration problem no retry will fix.
func classifyHTTP(status int, detail []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	err := fmt.Errorf("%s: %s", http.StatusText(status), truncate(string(detail), 200))
	if status == http.StatusTooManyRequests || status >= 500 {
		return MarkRetryable(err)
	}
	return err
}

// truncate keeps an error short enough to live in a table cell and a database
// column. An HTML error page is 10 kB of nothing.
func truncate(s string, n int) string {
	s = sanitizeHeader(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
