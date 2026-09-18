package store

import "testing"

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesMigrations(t *testing.T) {
	s := openTest(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != 3 {
		t.Fatalf("schema version = %d, want 3", v)
	}
	for _, table := range []string{"hosts", "samples", "samples_10m", "samples_1h", "samples_1d", "containers", "container_samples", "alert_rules", "alert_events", "deliveries", "azure_resources", "azure_costs", "azure_sync", "users", "sessions", "settings"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
}

func TestSchemaKeepsForeignKeys(t *testing.T) {
	s := openTest(t)
	for _, table := range []string{"samples", "samples_10m", "samples_1h", "samples_1d", "containers", "container_samples", "alert_events"} {
		rows, err := s.db.Query(`SELECT "table" FROM pragma_foreign_key_list(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for rows.Next() {
			var ref string
			rows.Scan(&ref)
			refs = append(refs, ref)
		}
		rows.Close()
		if len(refs) != 1 || refs[0] != "hosts" {
			t.Fatalf("%s must reference hosts on delete cascade, got %v", table, refs)
		}
	}
}

func TestOpenTwiceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sm.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.SchemaVersion(); v != 3 {
		t.Fatalf("version after reopen = %d", v)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	if err := s.SetSetting("agent_interval", "15"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetSetting("agent_interval")
	if err != nil || !ok || v != "15" {
		t.Fatalf("get = %q %v %v", v, ok, err)
	}
	if got := s.SettingInt("agent_interval", 10); got != 15 {
		t.Fatalf("SettingInt = %d", got)
	}
	if got := s.SettingInt("missing", 10); got != 10 {
		t.Fatalf("SettingInt default = %d", got)
	}
}

func TestDefaultSettingsSeeded(t *testing.T) {
	s := openTest(t)
	for key, want := range map[string]int{"agent_interval_sec": 10, "retention_raw_hours": 24, "retention_10m_days": 30, "retention_1h_days": 365} {
		if got := s.SettingInt(key, -1); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}
}
