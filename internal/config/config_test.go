package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrecedenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("listen: '127.0.0.1:9000'\nretention_days: 30\npublic_url: 'https://mail.example.com'\n"), 0600)
	c, cmd, e := Load([]string{"--config", path, "--listen", "127.0.0.1:9001", "serve"})
	if e != nil || c.Listen != "127.0.0.1:9001" || c.RetentionDays != 30 || !c.SecureCookie || len(cmd) != 1 {
		t.Fatal(c, cmd, e)
	}
	os.WriteFile(path, []byte("password: do-not-use\n"), 0600)
	if _, _, e = Load([]string{"--config", path}); e == nil {
		t.Fatal("unknown setting accepted")
	}
	if _, _, e = Load([]string{"--public-url", "https://mail.example.com/path"}); e == nil {
		t.Fatal("URL with path accepted")
	}
	if _, _, e = Load([]string{"--data-dir", "same", "--key-dir", "same"}); e == nil {
		t.Fatal("same data/key dir accepted")
	}
}

func TestAutomaticDefaultsAndOverrides(t *testing.T) {
	c, _, e := Load(nil)
	if e != nil || !c.AutomaticSync || c.SyncIntervalSeconds != 30 {
		t.Fatal(c, e)
	}
	path := filepath.Join(t.TempDir(), "automatic.yaml")
	os.WriteFile(path, []byte("automatic_sync: false\nsync_interval_seconds: 60\n"), 0600)
	c, _, e = Load([]string{"--config", path, "--automatic-sync=true", "--sync-interval-seconds", "5"})
	if e != nil || !c.AutomaticSync || c.SyncIntervalSeconds != 5 {
		t.Fatal(c, e)
	}
	for _, interval := range []string{"4", "3601"} {
		if _, _, e = Load([]string{"--sync-interval-seconds", interval}); e == nil {
			t.Fatal("invalid sync interval accepted")
		}
	}
}

func TestMCPOptionalDomainAndHTTPSRequirement(t *testing.T) {
	c, _, e := Load(nil)
	if e != nil || c.MCPEnabled || c.PublicURL != "" {
		t.Fatal(c, e)
	}
	for _, args := range [][]string{{"--mcp-enabled"}, {"--mcp-enabled", "--public-url", "http://mail.test"}} {
		if _, _, e = Load(args); e == nil {
			t.Fatal("insecure MCP accepted")
		}
	}
	c, _, e = Load([]string{"--mcp-enabled", "--public-url", "https://mail.test/"})
	if e != nil || !c.MCPEnabled || !c.SecureCookie || c.PublicURL != "https://mail.test" {
		t.Fatal(c, e)
	}
}
