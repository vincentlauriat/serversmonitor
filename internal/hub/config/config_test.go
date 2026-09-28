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

func TestTLSModes(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"SM_TLS_CERT": "/c.pem", "SM_TLS_KEY": "/k.pem"}))
	if err != nil || c.TLS.Mode() != "files" || c.Listen != ":443" || c.TLS.HTTPListen != ":80" || !c.Secure {
		t.Fatalf("files: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{"SM_TLS_DOMAINS": " Hub.Example.com, ,b.example.com", "SM_HTTP_LISTEN": "off", "SM_LISTEN": ":8443"}))
	if err != nil || c.TLS.Mode() != "acme" || c.Listen != ":8443" || c.TLS.HTTPListen != "" {
		t.Fatalf("acme: %+v %v", c, err)
	}
	if len(c.TLS.Domains) != 2 || c.TLS.Domains[0] != "hub.example.com" {
		t.Fatalf("domains = %v", c.TLS.Domains)
	}
	if c, _ := FromEnv(env(nil)); c.TLS.Mode() != "" || c.TLS.HTTPListen != "" {
		t.Fatalf("no TLS by default: %+v", c.TLS)
	}
}

func TestTLSMisconfigurations(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"cert without key":       {"SM_TLS_CERT": "/c.pem"},
		"key without cert":       {"SM_TLS_KEY": "/k.pem"},
		"files and acme":         {"SM_TLS_CERT": "/c.pem", "SM_TLS_KEY": "/k.pem", "SM_TLS_DOMAINS": "a.example"},
		"a redirect with no TLS": {"SM_HTTP_LISTEN": ":80"},
	} {
		if _, err := FromEnv(env(m)); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}
