package config

import (
	"log/slog"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8090" || c.DataDir != "./data" || c.Secure || c.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"SM_LISTEN": "127.0.0.1:9000", "SM_DATA_DIR": "/data", "SM_SECURE_COOKIES": "true", "SM_LOG_LEVEL": "debug"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || c.DataDir != "/data" || !c.Secure || c.LogLevel != slog.LevelDebug {
		t.Fatalf("config = %+v", c)
	}
}

func TestEmptyValueFallsBackToDefault(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"SM_LISTEN": ""}))
	if err != nil || c.Listen != ":8090" {
		t.Fatalf("empty env must not blank the listen address: %+v %v", c, err)
	}
}

func TestBadLogLevel(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{"SM_LOG_LEVEL": "loud"})); err == nil {
		t.Fatal("want error")
	}
}
