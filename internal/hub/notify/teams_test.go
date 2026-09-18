package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeCard walks the Adaptive Card envelope and returns the card content.
func decodeCard(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	if env["type"] != "message" {
		t.Fatalf(`envelope type = %v, want "message"`, env["type"])
	}
	atts, ok := env["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %v", env["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("contentType = %v", att["contentType"])
	}
	if v, present := att["contentUrl"]; !present || v != nil {
		t.Fatalf("contentUrl must be present and null, got %v (present=%v)", v, present)
	}
	return att["content"].(map[string]any)
}

func TestTeamsCardEnvelope(t *testing.T) {
	raw, err := json.Marshal(TeamsCard(fired()))
	if err != nil {
		t.Fatal(err)
	}
	card := decodeCard(t, raw)
	if card["$schema"] != "http://adaptivecards.io/schemas/adaptive-card.json" {
		t.Fatalf("$schema = %v", card["$schema"])
	}
	if card["type"] != "AdaptiveCard" {
		t.Fatalf("card type = %v", card["type"])
	}
	if card["version"] != "1.2" {
		// 1.2 is what the workflow connector guarantees; a higher version
		// renders as a blank card on some clients.
		t.Fatalf("version = %v", card["version"])
	}
	body, _ := card["body"].([]any)
	if len(body) == 0 {
		t.Fatal("card has no body")
	}
	flat, _ := json.Marshal(body)
	for _, want := range []string{"mac-vincent", "92.4%", "memory"} {
		if !strings.Contains(string(flat), want) {
			t.Fatalf("card body is missing %q: %s", want, flat)
		}
	}
}

func TestTeamsCardLinksWhenThereIsALink(t *testing.T) {
	raw, _ := json.Marshal(TeamsCard(fired()))
	if !strings.Contains(string(raw), "https://hub.example/hosts/1") {
		t.Fatalf("a configured link must reach the card: %s", raw)
	}
	m := fired()
	m.Link = ""
	raw, _ = json.Marshal(TeamsCard(m))
	card := decodeCard(t, raw)
	if _, has := card["actions"]; has {
		t.Fatalf("no link configured means no Open button: %s", raw)
	}
}

func TestTeamsCardStaysUnderTheSizeCap(t *testing.T) {
	// Teams rejects a message above 28 KB.
	m := fired()
	m.HostName = strings.Repeat("long-host-name-", 500)
	raw, _ := json.Marshal(TeamsCard(m))
	if len(raw) > 28*1024 {
		t.Fatalf("card is %d bytes, over the 28 KB cap", len(raw))
	}
}

func TestTeamsSendPostsTheCard(t *testing.T) {
	var body []byte
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		ctype = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted) // what a workflow actually answers
	}))
	defer srv.Close()
	ch := NewTeams(TeamsConfig{Enabled: true, URL: srv.URL})
	if ch.Name() != "teams" {
		t.Fatalf("name = %q", ch.Name())
	}
	if err := ch.Send(context.Background(), fired()); err != nil {
		t.Fatalf("a 202 is success: %v", err)
	}
	if ctype != "application/json" {
		t.Fatalf("content-type = %q", ctype)
	}
	decodeCard(t, body)
}

func TestTeamsThrottlingIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "rate limit")
	}))
	defer srv.Close()
	err := NewTeams(TeamsConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
	if !Retryable(err) {
		t.Fatalf("a 429 from Teams must be retried: %v", err)
	}
}

func TestTeamsExpiredWorkflowIsNotRetryable(t *testing.T) {
	// A deleted or expired workflow answers 4xx. Retrying it four times a minute
	// for every alert helps nobody.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := NewTeams(TeamsConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
	if err == nil || Retryable(err) {
		t.Fatalf("401 = %v (retryable %v)", err, Retryable(err))
	}
}

func TestEnabledBuildsOnlyWhatIsAskedFor(t *testing.T) {
	if got := Enabled(Config{}); len(got) != 0 {
		t.Fatalf("a fresh install notifies nobody, got %d channels", len(got))
	}
	c := Config{
		SMTP:    SMTPConfig{Enabled: true},
		Webhook: WebhookConfig{Enabled: true},
		Teams:   TeamsConfig{Enabled: true},
	}
	got := Enabled(c)
	if len(got) != 3 || got[0].Name() != "smtp" || got[1].Name() != "webhook" || got[2].Name() != "teams" {
		t.Fatalf("channels must come back in a stable order, got %v", names(got))
	}
	c.Webhook.Enabled = false
	if got := names(Enabled(c)); len(got) != 2 || got[0] != "smtp" || got[1] != "teams" {
		t.Fatalf("channels = %v", got)
	}
}

func names(cs []Channel) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name()
	}
	return out
}
