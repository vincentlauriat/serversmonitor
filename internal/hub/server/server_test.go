package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

type fakeAgents struct{ interval int }

func (f *fakeAgents) ServeHTTP(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) }
func (f *fakeAgents) Reconfigure(sec int)                              { f.interval = sec }
func (f *fakeAgents) Connected() []int64                               { return nil }

type rig struct {
	st     *store.Store
	srv    *httptest.Server
	client *http.Client
	bus    *Broadcaster
	agents *fakeAgents
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	static := fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}, "_app/x.js": {Data: []byte("js")}}
	r := &rig{st: st, bus: NewBroadcaster(), agents: &fakeAgents{}, now: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)}
	h := New(Deps{Store: st, Agents: r.agents, Bus: r.bus, Static: fs.FS(static), InstallScript: []byte("#!/bin/sh\necho hi\n"),
		Now: func() time.Time { return r.now }, Version: "test", Log: slog.Default(), SessionTTL: time.Hour})
	r.srv = httptest.NewServer(h)
	jar, _ := cookiejar.New(nil)
	r.client = &http.Client{Jar: jar}
	t.Cleanup(func() { r.srv.Close(); st.Close() })
	return r
}

func (r *rig) do(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func (r *rig) setupAndLogin(t *testing.T) {
	t.Helper()
	if resp, data := r.do(t, "POST", "/api/v1/setup", map[string]string{"email": "v@example.com", "password": "hunter22"}); resp.StatusCode != 201 {
		t.Fatalf("setup = %d %s", resp.StatusCode, data)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func (r *rig) newHost(t *testing.T, name string) (int64, string) {
	t.Helper()
	resp, data := r.do(t, "POST", "/api/v1/hosts", map[string]string{"name": name})
	if resp.StatusCode != 201 {
		t.Fatalf("create host = %d %s", resp.StatusCode, data)
	}
	var created struct {
		Host struct {
			ID int64 `json:"id"`
		} `json:"host"`
		Token string `json:"token"`
	}
	json.Unmarshal(data, &created)
	return created.Host.ID, created.Token
}

// newRule creates a real rule: an alert event whose rule no longer exists is
// deliberately filtered out of the firing set, so a test that fakes a rule id
// would prove nothing.
func (r *rig) newRule(t *testing.T, hostID *int64, metric string) int64 {
	t.Helper()
	rule, err := r.st.CreateRule(store.Rule{HostID: hostID, Metric: metric, Threshold: 90}, r.now)
	if err != nil {
		t.Fatal(err)
	}
	return rule.ID
}

func TestSetupThenLoginFlow(t *testing.T) {
	r := newRig(t)
	resp, data := r.do(t, "GET", "/api/v1/me", nil)
	if resp.StatusCode != 401 || !strings.Contains(string(data), `"setup_required":true`) {
		t.Fatalf("me before setup = %d %s", resp.StatusCode, data)
	}
	r.setupAndLogin(t)
	if resp, _ := r.do(t, "POST", "/api/v1/setup", map[string]string{"email": "x@y", "password": "12345678"}); resp.StatusCode != 403 {
		t.Fatalf("second setup = %d", resp.StatusCode)
	}
	if resp, data := r.do(t, "GET", "/api/v1/me", nil); resp.StatusCode != 200 || !strings.Contains(string(data), "v@example.com") {
		t.Fatalf("me = %d %s", resp.StatusCode, data)
	}
	r.do(t, "POST", "/api/v1/logout", nil)
	if resp, data := r.do(t, "GET", "/api/v1/me", nil); resp.StatusCode != 401 || strings.Contains(string(data), `"setup_required":true`) {
		t.Fatalf("logged out with a user present must not ask for setup: %d %s", resp.StatusCode, data)
	}
	if resp, _ := r.do(t, "GET", "/api/v1/hosts", nil); resp.StatusCode != 401 {
		t.Fatalf("after logout = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "POST", "/api/v1/login", map[string]string{"email": "V@example.com", "password": "hunter22"}); resp.StatusCode != 204 {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "GET", "/api/v1/hosts", nil); resp.StatusCode != 200 {
		t.Fatalf("hosts after login = %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	r.do(t, "POST", "/api/v1/logout", nil)
	for i := 0; i < 5; i++ {
		if resp, _ := r.do(t, "POST", "/api/v1/login", map[string]string{"email": "v@example.com", "password": "wrong"}); resp.StatusCode != 401 {
			t.Fatalf("attempt %d = %d", i, resp.StatusCode)
		}
	}
	if resp, _ := r.do(t, "POST", "/api/v1/login", map[string]string{"email": "v@example.com", "password": "hunter22"}); resp.StatusCode != 429 {
		t.Fatalf("want 429, got %d", resp.StatusCode)
	}
}

func TestLoginUnknownEmailIsUnauthorizedNot500(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	r.do(t, "POST", "/api/v1/logout", nil)
	if resp, data := r.do(t, "POST", "/api/v1/login", map[string]string{"email": "ghost@example.com", "password": "whatever1"}); resp.StatusCode != 401 {
		t.Fatalf("unknown email = %d %s", resp.StatusCode, data)
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	r := newRig(t)
	if resp, _ := r.do(t, "POST", "/api/v1/setup", map[string]string{"email": "v@example.com", "password": "short"}); resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestHostsCRUD(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	resp, data := r.do(t, "POST", "/api/v1/hosts", map[string]string{"name": "pi"})
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, data)
	}
	var created struct {
		Host struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"host"`
		Token   string `json:"token"`
		Install string `json:"install"`
	}
	json.Unmarshal(data, &created)
	if created.Token == "" || !strings.Contains(created.Install, created.Token) || !strings.Contains(created.Install, "install.sh") || created.Host.Status != "never_seen" {
		t.Fatalf("created = %+v", created)
	}
	if resp, _ := r.do(t, "POST", "/api/v1/hosts", map[string]string{"name": "pi"}); resp.StatusCode != 409 {
		t.Fatalf("duplicate = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "POST", "/api/v1/hosts", map[string]string{"name": "  "}); resp.StatusCode != 400 {
		t.Fatalf("blank name = %d", resp.StatusCode)
	}
	id := created.Host.ID
	cpu := 55.0
	r.st.InsertSample(id, &proto.Sample{Type: proto.TypeSample, At: r.now.Add(-time.Second), CPU: &cpu, Disks: []proto.Disk{{Mount: "/", Used: 1, Total: 4}}})
	_, data = r.do(t, "GET", "/api/v1/hosts", nil)
	var list []map[string]any
	json.Unmarshal(data, &list)
	if len(list) != 1 {
		t.Fatalf("list = %s", data)
	}
	latest := list[0]["latest"].(map[string]any)
	if latest["cpu"] != 55.0 || latest["mem_used"] != nil {
		t.Fatalf("latest = %v (mem_used must be null, not 0)", latest)
	}
	resp, data = r.do(t, "PATCH", "/api/v1/hosts/"+itoa(id), map[string]any{"name": "pi2", "muted": true})
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"name":"pi2"`) || !strings.Contains(string(data), `"muted":true`) {
		t.Fatalf("patch = %d %s", resp.StatusCode, data)
	}
	resp, data = r.do(t, "POST", "/api/v1/hosts/"+itoa(id)+"/token", nil)
	if resp.StatusCode != 200 || strings.Contains(string(data), created.Token) {
		t.Fatalf("regenerate = %d %s", resp.StatusCode, data)
	}
	resp, data = r.do(t, "GET", "/api/v1/hosts/"+itoa(id)+"/series?period=1h", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"cpu":55`) {
		t.Fatalf("series = %d %s", resp.StatusCode, data)
	}
	if resp, _ := r.do(t, "GET", "/api/v1/hosts/"+itoa(id)+"/series?period=2w", nil); resp.StatusCode != 400 {
		t.Fatalf("bad period = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "DELETE", "/api/v1/hosts/"+itoa(id), nil); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil); resp.StatusCode != 404 {
		t.Fatalf("after delete = %d", resp.StatusCode)
	}
}

func TestHostFiringCountIsReported(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	id, _ := r.newHost(t, "pi")
	cpuRule := r.newRule(t, &id, "cpu")
	diskRule := r.newRule(t, &id, "disk")
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: cpuRule, HostID: id, Metric: "cpu", Kind: "fired", Value: 95, At: r.now})
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: diskRule, HostID: id, Metric: "disk", Kind: "fired", Value: 95, At: r.now})
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: diskRule, HostID: id, Metric: "disk", Kind: "resolved", Value: 10, At: r.now.Add(time.Minute)})
	_, data := r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	var h map[string]any
	json.Unmarshal(data, &h)
	if h["firing"] != 1.0 {
		t.Fatalf("firing = %v, want 1 (a resolved alert must not count)", h["firing"])
	}
}

func TestContainersEndpoint(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	id, _ := r.newHost(t, "pi")
	sm := &proto.Sample{Type: proto.TypeSample, At: r.now, DockerAvailable: true,
		Containers: []proto.Container{{Name: "web", Image: "nginx", Status: "running", CPU: 3, MemUsed: 50}}}
	r.st.InsertSample(id, sm)
	resp, data := r.do(t, "GET", "/api/v1/hosts/"+itoa(id)+"/containers", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"name":"web"`) {
		t.Fatalf("containers = %d %s", resp.StatusCode, data)
	}
}

func TestAlertsRulesAndEvents(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	r.st.SeedDefaultRules(r.now)
	id, _ := r.newHost(t, "pi")
	seeded, err := r.st.ListRules()
	if err != nil || len(seeded) == 0 {
		t.Fatalf("seed = %v %v", seeded, err)
	}
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: seeded[0].ID, HostID: id, Metric: "cpu", Kind: "fired", Value: 95, At: r.now})
	resp, data := r.do(t, "GET", "/api/v1/alerts", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"host_name":"pi"`) || !strings.Contains(string(data), `"metric":"cpu"`) {
		t.Fatalf("alerts = %d %s", resp.StatusCode, data)
	}
	resp, data = r.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"host_id": id, "metric": "disk", "threshold": 95, "duration_sec": 60})
	if resp.StatusCode != 201 {
		t.Fatalf("create rule = %d %s", resp.StatusCode, data)
	}
	var rule struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(data, &rule)
	if resp, _ := r.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"metric": "teleport", "threshold": 1, "duration_sec": 0}); resp.StatusCode != 400 {
		t.Fatalf("bad metric = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/alerts/rules/"+itoa(rule.ID), map[string]any{"host_id": id, "metric": "disk", "threshold": 97, "duration_sec": 60}); resp.StatusCode != 200 {
		t.Fatalf("update rule = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/alerts/rules/99999", map[string]any{"metric": "disk", "threshold": 97, "duration_sec": 60}); resp.StatusCode != 404 {
		t.Fatalf("update of a missing rule = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "DELETE", "/api/v1/alerts/rules/"+itoa(rule.ID), nil); resp.StatusCode != 204 {
		t.Fatalf("delete rule = %d", resp.StatusCode)
	}
	resp, data = r.do(t, "GET", "/api/v1/alerts/events?page=1", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"kind":"fired"`) {
		t.Fatalf("events = %d %s", resp.StatusCode, data)
	}
}

func TestSettingsReconfigure(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	resp, data := r.do(t, "GET", "/api/v1/settings", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"agent_interval_sec":10`) || !strings.Contains(string(data), `"version":"test"`) {
		t.Fatalf("settings = %d %s", resp.StatusCode, data)
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/settings", map[string]int{"agent_interval_sec": 30, "retention_raw_hours": 48, "retention_10m_days": 30, "retention_1h_days": 365}); resp.StatusCode != 204 {
		t.Fatalf("put = %d", resp.StatusCode)
	}
	if r.agents.interval != 30 || r.st.SettingInt("agent_interval_sec", 0) != 30 || r.st.SettingInt("retention_raw_hours", 0) != 48 {
		t.Fatal("settings not applied")
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/settings", map[string]int{"agent_interval_sec": 0}); resp.StatusCode != 400 {
		t.Fatalf("interval 0 = %d", resp.StatusCode)
	}
}

func TestPasswordChange(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	if resp, _ := r.do(t, "PUT", "/api/v1/password", map[string]string{"current": "wrong", "new": "newpassword1"}); resp.StatusCode != 403 {
		t.Fatalf("wrong current = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/password", map[string]string{"current": "hunter22", "new": "short"}); resp.StatusCode != 400 {
		t.Fatalf("short new = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/password", map[string]string{"current": "hunter22", "new": "newpassword1"}); resp.StatusCode != 204 {
		t.Fatalf("change = %d", resp.StatusCode)
	}
	r.do(t, "POST", "/api/v1/logout", nil)
	if resp, _ := r.do(t, "POST", "/api/v1/login", map[string]string{"email": "v@example.com", "password": "newpassword1"}); resp.StatusCode != 204 {
		t.Fatalf("login with new = %d", resp.StatusCode)
	}
}

func TestSSEStreamsEvents(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	req, _ := http.NewRequest("GET", r.srv.URL+"/api/v1/events", nil)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		for i := 0; i < 5; i++ {
			r.bus.Publish("host", map[string]any{"id": 7})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	sc := bufio.NewScanner(resp.Body)
	done := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") && strings.Contains(line, `"id":7`) {
				done <- true
				return
			}
		}
		done <- false
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("stream ended without the event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE event")
	}
}

func TestSSERequiresLogin(t *testing.T) {
	r := newRig(t)
	if resp, _ := r.do(t, "GET", "/api/v1/events", nil); resp.StatusCode != 401 {
		t.Fatalf("anonymous SSE = %d", resp.StatusCode)
	}
}

func TestStaticAndInstallScript(t *testing.T) {
	r := newRig(t)
	resp, data := r.do(t, "GET", "/install.sh", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(data), "#!/bin/sh") {
		t.Fatalf("install.sh = %d %s", resp.StatusCode, data)
	}
	if _, data := r.do(t, "GET", "/_app/x.js", nil); string(data) != "js" {
		t.Fatalf("static = %s", data)
	}
	if _, data := r.do(t, "GET", "/hosts/42", nil); string(data) != "<html>app</html>" {
		t.Fatalf("spa fallback = %s", data)
	}
	if resp, _ := r.do(t, "GET", "/agent/ws", nil); resp.StatusCode != 418 {
		t.Fatalf("agent route = %d", resp.StatusCode)
	}
}

// A muted host and a deleted rule both leave a "fired" row that can never be
// resolved: the evaluator skips a muted host entirely, and a deleted rule has
// nothing left to resolve it. The badge must not stay lit forever.
func TestMutedHostStopsCountingAsFiring(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	id, _ := r.newHost(t, "pi")
	rule := r.newRule(t, &id, "cpu")
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: rule, HostID: id, Metric: "cpu", Kind: "fired", Value: 95, At: r.now})
	_, data := r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	var h map[string]any
	json.Unmarshal(data, &h)
	if h["firing"] != 1.0 {
		t.Fatalf("firing before muting = %v, want 1", h["firing"])
	}
	if resp, _ := r.do(t, "PATCH", "/api/v1/hosts/"+itoa(id), map[string]any{"muted": true}); resp.StatusCode != 200 {
		t.Fatalf("mute = %d", resp.StatusCode)
	}
	_, data = r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	json.Unmarshal(data, &h)
	if h["firing"] != 0.0 {
		t.Fatalf("a muted host must show no firing alert, got %v", h["firing"])
	}
	_, data = r.do(t, "GET", "/api/v1/alerts", nil)
	var alerts struct {
		Firing []map[string]any `json:"firing"`
	}
	json.Unmarshal(data, &alerts)
	if len(alerts.Firing) != 0 {
		t.Fatalf("the alerts page must not list a muted host: %v", alerts.Firing)
	}
	// Unmuting brings it back: the log was never rewritten.
	r.do(t, "PATCH", "/api/v1/hosts/"+itoa(id), map[string]any{"muted": false})
	_, data = r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	json.Unmarshal(data, &h)
	if h["firing"] != 1.0 {
		t.Fatalf("unmuting must restore the alert, got %v", h["firing"])
	}
}

func TestDeletedRuleStopsCountingAsFiring(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	id, _ := r.newHost(t, "pi")
	resp, data := r.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"host_id": id, "metric": "cpu", "threshold": 1, "duration_sec": 0})
	if resp.StatusCode != 201 {
		t.Fatalf("create rule = %d", resp.StatusCode)
	}
	var rule struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(data, &rule)
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: rule.ID, HostID: id, Metric: "cpu", Kind: "fired", Value: 95, At: r.now})
	var h map[string]any
	_, data = r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	json.Unmarshal(data, &h)
	if h["firing"] != 1.0 {
		t.Fatalf("firing before delete = %v", h["firing"])
	}
	if resp, _ := r.do(t, "DELETE", "/api/v1/alerts/rules/"+itoa(rule.ID), nil); resp.StatusCode != 204 {
		t.Fatalf("delete rule = %d", resp.StatusCode)
	}
	_, data = r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	json.Unmarshal(data, &h)
	if h["firing"] != 0.0 {
		t.Fatalf("a deleted rule must stop counting, got %v", h["firing"])
	}
	// The log itself keeps the event: it says what really happened.
	_, data = r.do(t, "GET", "/api/v1/alerts/events", nil)
	if !strings.Contains(string(data), `"kind":"fired"`) {
		t.Fatalf("the append-only log must keep the event: %s", data)
	}
}

func TestOfflineAlertSurvivesTheLiveRuleCheck(t *testing.T) {
	// The implicit offline rule has id 0 and no row in alert_rules; the filter
	// must not treat it as deleted.
	r := newRig(t)
	r.setupAndLogin(t)
	id, _ := r.newHost(t, "pi")
	r.st.InsertAlertEvent(store.AlertEvent{RuleID: 0, HostID: id, Metric: "status", Kind: "fired", Value: 1, At: r.now})
	_, data := r.do(t, "GET", "/api/v1/hosts/"+itoa(id), nil)
	var h map[string]any
	json.Unmarshal(data, &h)
	if h["firing"] != 1.0 {
		t.Fatalf("the offline alert must count, got %v", h["firing"])
	}
}
