package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleChannelsThroughTheAPI(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	post := func(channels any) (int, string) {
		body := map[string]any{"metric": "cpu", "threshold": 90, "duration_sec": 60}
		if channels != "absent" {
			body["channels"] = channels
		}
		resp, data := r.do(t, "POST", "/api/v1/alerts/rules", body)
		return resp.StatusCode, string(data)
	}
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"absent is every channel", "absent", `"channels":null`},
		{"null is every channel", nil, `"channels":null`},
		{"empty is none", []string{}, `"channels":[]`},
		{"order and duplicates are normalised", []string{"teams", "smtp", "teams"}, `"channels":["smtp","teams"]`},
	} {
		code, data := post(tc.in)
		if code != 201 || !strings.Contains(data, tc.want) {
			t.Errorf("%s: %d %s, want %s", tc.name, code, data, tc.want)
		}
	}
	code, data := post([]string{"pager"})
	if code != 400 || !strings.Contains(data, "unknown channel") {
		t.Fatalf("unknown channel = %d %s", code, data)
	}

	// What was stored is what the list says.
	_, list := r.do(t, "GET", "/api/v1/alerts", nil)
	var got struct {
		Rules []struct {
			Channels *[]string `json:"channels"`
		} `json:"rules"`
	}
	json.Unmarshal(list, &got)
	var nulls, empties, some int
	for _, rule := range got.Rules {
		switch {
		case rule.Channels == nil:
			nulls++
		case len(*rule.Channels) == 0:
			empties++
		default:
			some++
		}
	}
	// The seeded defaults have no route either.
	if empties != 1 || some != 1 || nulls < 2 {
		t.Fatalf("stored routes: %d null, %d empty, %d set", nulls, empties, some)
	}
}

func TestNotificationRoutesRoundTrip(t *testing.T) {
	r := newNotifyRig(t)
	_, data := r.do(t, "GET", "/api/v1/notifications", nil)
	if !strings.Contains(string(data), `"offline_channels":null`) || !strings.Contains(string(data), `"guardrail_channels":null`) {
		t.Fatalf("a fresh install routes everywhere: %s", data)
	}
	resp, data := r.do(t, "PUT", "/api/v1/notifications", map[string]any{
		"offline_channels": []string{"teams", "smtp"}, "guardrail_channels": []string{}})
	if resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	_, data = r.do(t, "GET", "/api/v1/notifications", nil)
	if !strings.Contains(string(data), `"offline_channels":["smtp","teams"]`) || !strings.Contains(string(data), `"guardrail_channels":[]`) {
		t.Fatalf("routes lost: %s", data)
	}
	resp, data = r.do(t, "PUT", "/api/v1/notifications", map[string]any{"offline_channels": []string{"pager"}})
	if resp.StatusCode != 400 || !strings.Contains(string(data), "offline routing") {
		t.Fatalf("unknown channel = %d %s", resp.StatusCode, data)
	}
}
