package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleRemindMinutes(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	resp, data := r.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"metric": "disk", "threshold": 90, "duration_sec": 60, "remind_minutes": 240})
	if resp.StatusCode != 201 || !strings.Contains(string(data), `"remind_minutes":240`) {
		t.Fatalf("create = %d %s", resp.StatusCode, data)
	}
	for _, bad := range []int{-1, 10081} {
		if resp, _ := r.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"metric": "disk", "threshold": 90, "duration_sec": 60, "remind_minutes": bad}); resp.StatusCode != 400 {
			t.Errorf("remind %d = %d", bad, resp.StatusCode)
		}
	}
}

func TestQuietHoursAndRemindersThroughNotifications(t *testing.T) {
	r := newNotifyRig(t)
	nights := []map[string]any{{"days": []int{1, 2, 3, 4, 5, 6, 7}, "from": "22:00", "to": "07:00"}}
	resp, data := r.do(t, "PUT", "/api/v1/notifications", map[string]any{
		"offline_remind_minutes": 30, "guardrail_remind_minutes": 1440,
		"quiet": map[string]any{"smtp": nights, "teams": []any{}},
	})
	if resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	_, data = r.do(t, "GET", "/api/v1/notifications", nil)
	var v struct {
		Offline   int64                       `json:"offline_remind_minutes"`
		Guardrail int64                       `json:"guardrail_remind_minutes"`
		Quiet     map[string][]map[string]any `json:"quiet"`
		Zone      string                      `json:"quiet_zone"`
	}
	json.Unmarshal(data, &v)
	if v.Offline != 30 || v.Guardrail != 1440 || len(v.Quiet["smtp"]) != 1 || v.Quiet["teams"] == nil || v.Zone == "" {
		t.Fatalf("notifications = %s", data)
	}

	allWeek := []map[string]any{{"days": []int{1, 2, 3, 4, 5, 6, 7}, "from": "00:00", "to": "24:00"}}
	resp, data = r.do(t, "PUT", "/api/v1/notifications", map[string]any{"quiet": map[string]any{"smtp": allWeek}})
	if resp.StatusCode != 400 || !strings.Contains(string(data), "whole week") {
		t.Fatalf("whole week = %d %s", resp.StatusCode, data)
	}
	resp, data = r.do(t, "PUT", "/api/v1/notifications", map[string]any{"quiet": map[string]any{"pager": nights}})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown channel = %d %s", resp.StatusCode, data)
	}
	resp, _ = r.do(t, "PUT", "/api/v1/notifications", map[string]any{"quiet": map[string]any{"smtp": []map[string]any{{"days": []int{9}, "from": "22:00", "to": "07:00"}}}})
	if resp.StatusCode != 400 {
		t.Fatalf("day 9 = %d", resp.StatusCode)
	}
}
