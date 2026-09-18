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

func TestWebhookPostsTheDocumentedBody(t *testing.T) {
	var got struct {
		method, path, ctype, auth, ntfyTitle string
		body                                 WebhookPayload
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.ctype = r.Header.Get("Content-Type")
		got.auth = r.Header.Get("Authorization")
		got.ntfyTitle = r.Header.Get("X-Title")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got.body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL + "/hooks/sm",
		Headers: map[string]string{"Authorization": "Bearer tok"}})
	if ch.Name() != "webhook" {
		t.Fatalf("name = %q", ch.Name())
	}
	if err := ch.Send(context.Background(), fired()); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/hooks/sm" {
		t.Fatalf("%s %s", got.method, got.path)
	}
	if got.ctype != "application/json" {
		t.Fatalf("content-type = %q", got.ctype)
	}
	if got.auth != "Bearer tok" {
		t.Fatalf("configured headers must be sent, got %q", got.auth)
	}
	if got.ntfyTitle != "mac-vincent memory 92.4% (fired)" {
		// ntfy reads the title from a header, not the body.
		t.Fatalf("X-Title = %q", got.ntfyTitle)
	}
	b := got.body
	if b.Host != "mac-vincent" || b.Metric != "memory" || b.Kind != "fired" || b.Value != 92.4 ||
		b.Threshold != 90 || b.Priority != 4 || b.Link != "https://hub.example/hosts/1" {
		t.Fatalf("payload = %+v", b)
	}
	if len(b.Tags) != 1 || b.Tags[0] != "rotating_light" {
		t.Fatalf("tags = %v", b.Tags)
	}
	if !strings.Contains(b.Message, "92.4%") {
		t.Fatalf("message = %q", b.Message)
	}
}

func TestWebhookRetryClassification(t *testing.T) {
	cases := []struct {
		status  int
		wantErr bool
		retry   bool
	}{
		{200, false, false},
		{204, false, false},
		{400, true, false}, // our payload is wrong; retrying changes nothing
		{404, true, false},
		{429, true, true}, // asked to slow down
		{500, true, true},
		{503, true, true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			io.WriteString(w, "detail from the server")
		}))
		err := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
		srv.Close()
		if (err != nil) != c.wantErr {
			t.Fatalf("status %d: err = %v, want error = %v", c.status, err, c.wantErr)
		}
		if err != nil && Retryable(err) != c.retry {
			t.Fatalf("status %d: retryable = %v, want %v", c.status, Retryable(err), c.retry)
		}
		if err != nil && !strings.Contains(err.Error(), "detail from the server") {
			t.Fatalf("status %d: the server's own words are the only clue Vincent gets: %v", c.status, err)
		}
	}
}

func TestWebhookErrorBodyIsTruncated(t *testing.T) {
	// An HTML error page must not end up whole in last_error, and from there in
	// the settings table.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, strings.Repeat("x", 10000))
	}))
	defer srv.Close()
	err := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL}).Send(context.Background(), fired())
	if err == nil {
		t.Fatal("want error")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error message is %d bytes", len(err.Error()))
	}
}

func TestWebhookUnreachableIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing listens there
	err := NewWebhook(WebhookConfig{Enabled: true, URL: url}).Send(context.Background(), fired())
	if !Retryable(err) {
		t.Fatalf("a dead endpoint is worth retrying: %v", err)
	}
}

func TestWebhookHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if err := NewWebhook(WebhookConfig{Enabled: true, URL: srv.URL}).Send(ctx, fired()); err == nil {
		t.Fatal("a cancelled context must not send")
	}
}
