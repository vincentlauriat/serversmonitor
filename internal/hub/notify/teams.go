package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// teamsMaxHostName bounds the one field that comes from user input, so a long
// name cannot push the card past the 28 KB Teams accepts.
const teamsMaxHostName = 200

// TeamsCard builds the Adaptive Card envelope a Power Automate workflow expects.
//
// The connector format (MessageCard, themeColor) was disabled between 18 and
// 22 May 2026 and must not come back. Verified against Microsoft Learn on
// 2026-09-18: 28 KB message cap, more than 4 requests per second throttled
// with a 429, contentUrl required and null.
func TeamsCard(m Message) map[string]any {
	title := "🔴 " + m.Title()
	if !m.Fired() {
		title = "🟢 " + m.Title()
	}
	facts := []map[string]any{
		{"title": "Host", "value": truncate(m.HostName, teamsMaxHostName)},
		{"title": "Time", "value": m.At.UTC().Format("2006-01-02 15:04:05 UTC")},
	}
	if v := m.ValueText(); v != "" {
		facts = append(facts,
			map[string]any{"title": "Value", "value": v},
			map[string]any{"title": "Rule", "value": fmt.Sprintf("above %g for %s", m.Threshold, shortDuration(m.Duration))})
	} else {
		facts = append(facts, map[string]any{"title": "Rule", "value": "no sample for three intervals"})
	}
	content := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.2",
		"body": []any{
			map[string]any{"type": "TextBlock", "text": truncate(title, teamsMaxHostName+80),
				"weight": "Bolder", "size": "Medium", "wrap": true},
			map[string]any{"type": "FactSet", "facts": facts},
		},
	}
	if m.Link != "" {
		content["actions"] = []any{
			map[string]any{"type": "Action.OpenUrl", "title": "Open in ServersMonitor", "url": m.Link},
		}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{
			map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"contentUrl":  nil, // required by the workflow connector, and it must be null
				"content":     content,
			},
		},
	}
}

type Teams struct {
	cfg    TeamsConfig
	client *http.Client
}

func NewTeams(cfg TeamsConfig) *Teams {
	return &Teams{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}
}

func (t *Teams) Name() string { return "teams" }

func (t *Teams) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(TeamsCard(m))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ServersMonitor")
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return MarkRetryable(fmt.Errorf("teams post: %w", err))
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return classifyHTTP(resp.StatusCode, detail)
}
