package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type fakeNotifier struct {
	reloads int
	tested  []string
	err     error
}

func (f *fakeNotifier) ReloadNotify() { f.reloads++ }

func (f *fakeNotifier) TestNotify(_ context.Context, channel string) error {
	f.tested = append(f.tested, channel)
	return f.err
}

func newNotifyRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.setupAndLogin(t)
	return r
}

func (r *rig) notifier(t *testing.T) *fakeNotifier {
	t.Helper()
	f, ok := r.notify.(*fakeNotifier)
	if !ok {
		t.Fatalf("rig notifier is %T", r.notify)
	}
	return f
}

func TestNotificationsDefaultsAreAllOff(t *testing.T) {
	r := newNotifyRig(t)
	resp, data := r.do(t, "GET", "/api/v1/notifications", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get = %d %s", resp.StatusCode, data)
	}
	var view map[string]any
	json.Unmarshal(data, &view)
	for _, k := range []string{"smtp_enabled", "webhook_enabled", "teams_enabled", "smtp_password_set"} {
		if view[k] != false {
			t.Errorf("%s = %v on a fresh install", k, view[k])
		}
	}
	if _, leaked := view["smtp_password"]; leaked {
		t.Fatal("the password must never appear in a response")
	}
}

func TestNotificationsRoundTripHidesThePassword(t *testing.T) {
	r := newNotifyRig(t)
	body := map[string]any{"public_url": "https://hub.example", "smtp_enabled": true,
		"smtp_host": "smtp.example", "smtp_port": 587, "smtp_username": "u",
		"smtp_password": "secret", "smtp_from": "hub@example",
		"smtp_to": []string{"a@example"}, "smtp_tls": "starttls"}
	if resp, data := r.do(t, "PUT", "/api/v1/notifications", body); resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	if n := r.notifier(t).reloads; n != 1 {
		t.Fatalf("saving settings must reload the channels, reloads = %d", n)
	}
	_, data := r.do(t, "GET", "/api/v1/notifications", nil)
	var view map[string]any
	json.Unmarshal(data, &view)
	if view["smtp_password_set"] != true {
		t.Fatal("smtp_password_set must say a password exists")
	}
	if _, leaked := view["smtp_password"]; leaked {
		t.Fatal("the password leaked into the response")
	}
	if view["smtp_username"] != "u" || view["smtp_host"] != "smtp.example" {
		t.Fatalf("view = %v", view)
	}
}

func TestAbsentPasswordKeepsTheStoredOne(t *testing.T) {
	// The browser never receives the password, so it cannot send it back. A save
	// that omits the field must not silently erase it.
	r := newNotifyRig(t)
	base := map[string]any{"smtp_enabled": true, "smtp_host": "h", "smtp_port": 25,
		"smtp_from": "f@example", "smtp_to": []string{"a@example"}, "smtp_tls": "none"}
	with := map[string]any{}
	for k, v := range base {
		with[k] = v
	}
	with["smtp_password"] = "secret"
	r.do(t, "PUT", "/api/v1/notifications", with)

	base["smtp_host"] = "h2"
	if resp, data := r.do(t, "PUT", "/api/v1/notifications", base); resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	got, _, _ := r.st.GetSetting("notify_smtp_password")
	if got != "secret" {
		t.Fatalf("password = %q, want the stored one kept", got)
	}
	_, data := r.do(t, "GET", "/api/v1/notifications", nil)
	var view map[string]any
	json.Unmarshal(data, &view)
	if view["smtp_host"] != "h2" {
		t.Fatalf("the rest of the save must still apply, host = %v", view["smtp_host"])
	}
}

func TestEmptyPasswordClearsIt(t *testing.T) {
	r := newNotifyRig(t)
	body := map[string]any{"smtp_enabled": true, "smtp_host": "h", "smtp_port": 25,
		"smtp_from": "f@example", "smtp_to": []string{"a@example"}, "smtp_tls": "none",
		"smtp_password": "secret"}
	r.do(t, "PUT", "/api/v1/notifications", body)
	body["smtp_password"] = ""
	r.do(t, "PUT", "/api/v1/notifications", body)
	if got, _, _ := r.st.GetSetting("notify_smtp_password"); got != "" {
		t.Fatalf("an explicit empty string must clear the password, got %q", got)
	}
}

func TestInvalidConfigIsRejectedWithItsReason(t *testing.T) {
	// Each case checks its own setting immediately after its own rejected save.
	// Checking once at the end would not catch a handler that writes before
	// validating: a later case overwrites the key with an empty value and the
	// assertion passes for the wrong reason. Verified by mutation.
	cases := []struct {
		name string
		body map[string]any
		key  string // the setting this case would have written
	}{
		{"smtp with no recipient", map[string]any{"smtp_enabled": true, "smtp_host": "h",
			"smtp_port": 25, "smtp_from": "f", "smtp_to": []string{}, "smtp_tls": "none"}, "notify_smtp_host"},
		{"teams over http", map[string]any{"teams_enabled": true,
			"teams_url": "http://example/x"}, "notify_teams_url"},
		{"public url not a url", map[string]any{"public_url": "hub.example"}, "notify_public_url"},
		{"bad tls mode", map[string]any{"smtp_enabled": true, "smtp_host": "h", "smtp_port": 25,
			"smtp_from": "f", "smtp_to": []string{"a@example"}, "smtp_tls": "ssl"}, "notify_smtp_host"},
	}
	for _, c := range cases {
		r := newNotifyRig(t) // a fresh store per case, so nothing leaks between them
		resp, data := r.do(t, "PUT", "/api/v1/notifications", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%s)", c.name, resp.StatusCode, data)
			continue
		}
		if len(data) < 10 {
			t.Errorf("%s: the reason must reach the browser, got %q", c.name, data)
		}
		if v, ok, _ := r.st.GetSetting(c.key); ok && v != "" {
			t.Errorf("%s: a rejected save must write nothing, %s = %q", c.name, c.key, v)
		}
		if n := r.notifier(t).reloads; n != 0 {
			t.Errorf("%s: a rejected save must not reload anything, reloads = %d", c.name, n)
		}
	}
}

func TestTestEndpointReportsTheRealFailure(t *testing.T) {
	r := newNotifyRig(t)
	r.notifier(t).err = errors.New("Bad Gateway: workflow not found")
	resp, data := r.do(t, "POST", "/api/v1/notifications/test", map[string]string{"channel": "teams"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(data), "workflow not found") {
		t.Fatalf("the reason is the whole point of a test button: %s", data)
	}
	if tested := r.notifier(t).tested; len(tested) != 1 || tested[0] != "teams" {
		t.Fatalf("tested = %v", tested)
	}
}

func TestTestEndpointSucceeds(t *testing.T) {
	r := newNotifyRig(t)
	resp, data := r.do(t, "POST", "/api/v1/notifications/test", map[string]string{"channel": "smtp"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("code = %d %s", resp.StatusCode, data)
	}
	if tested := r.notifier(t).tested; tested[0] != "smtp" {
		t.Fatalf("tested = %v", tested)
	}
}

func TestTestEndpointRejectsAnUnknownChannel(t *testing.T) {
	r := newNotifyRig(t)
	resp, _ := r.do(t, "POST", "/api/v1/notifications/test", map[string]string{"channel": "carrier-pigeon"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", resp.StatusCode)
	}
	if tested := r.notifier(t).tested; len(tested) != 0 {
		t.Fatalf("an unknown channel must not reach the hub, tested = %v", tested)
	}
}

func TestDeliveriesListShowsTheOutcome(t *testing.T) {
	r := newNotifyRig(t)
	hostID, _ := r.newHost(t, "pi")
	ruleID := r.newRule(t, nil, "cpu")
	e, err := r.st.InsertAlertEvent(store.AlertEvent{RuleID: ruleID, HostID: hostID,
		Metric: "cpu", Kind: "fired", Value: 99, At: r.now})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.st.CreateDelivery(e.ID, "webhook", r.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.MarkDeliveryFailed(d.ID, r.now, 4, "503 Service Unavailable"); err != nil {
		t.Fatal(err)
	}
	_, data := r.do(t, "GET", "/api/v1/notifications/deliveries", nil)
	var out []map[string]any
	json.Unmarshal(data, &out)
	if len(out) != 1 {
		t.Fatalf("deliveries = %s", data)
	}
	if out[0]["state"] != "failed" || out[0]["channel"] != "webhook" ||
		out[0]["last_error"] != "503 Service Unavailable" || out[0]["host"] != "pi" ||
		out[0]["kind"] != "fired" {
		t.Fatalf("delivery view = %v", out[0])
	}
}

func TestNotificationsRequireAuth(t *testing.T) {
	r := newRig(t) // deliberately not logged in
	for _, path := range []string{"/api/v1/notifications", "/api/v1/notifications/deliveries"} {
		if resp, _ := r.do(t, "GET", path, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", path, resp.StatusCode)
		}
	}
}
