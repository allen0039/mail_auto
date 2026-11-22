package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen              string `yaml:"listen"`
	DataDir             string `yaml:"data_dir"`
	KeyDir              string `yaml:"key_dir"`
	SecretDir           string `yaml:"secret_dir"`
	PublicURL           string `yaml:"public_url"`
	SecureCookie        bool   `yaml:"secure_cookie"`
	SessionHours        int    `yaml:"session_hours"`
	RetentionDays       int    `yaml:"retention_days"`
	AutomaticSync       bool   `yaml:"automatic_sync"`
	SyncIntervalSeconds int    `yaml:"sync_interval_seconds"`
	MCPEnabled          bool   `yaml:"mcp_enabled"`
}

func Load(args []string) (Config, []string, error) {
	c := Config{Listen: "127.0.0.1:8080", DataDir: "./data", KeyDir: "./keys", SecretDir: "/run/secrets", SessionHours: 12, RetentionDays: 0, AutomaticSync: true, SyncIntervalSeconds: 30}
	// Locate the file first; flag parsing below preserves explicit flag precedence.
	var path string
	for i, a := range args {
		if a == "--config" || a == "-config" {
			if i+1 >= len(args) {
				return c, nil, errors.New("--config requires a path")
			}
			path = args[i+1]
		} else if strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-config=") {
			_, path, _ = strings.Cut(a, "=")
		}
	}
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return c, nil, e
		}
		defer f.Close()
		d := yaml.NewDecoder(io.LimitReader(f, 64<<10))
		d.KnownFields(true)
		if e = d.Decode(&c); e != nil {
			return c, nil, e
		}
		var extra any
		if e = d.Decode(&extra); e != io.EOF {
			return c, nil, errors.New("config must contain one YAML document")
		}
	}
	fs := flag.NewFlagSet("mail-auto", flag.ContinueOnError)
	fs.StringVar(&path, "config", path, "optional YAML configuration")
	fs.StringVar(&c.Listen, "listen", c.Listen, "HTTP listen address")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "database directory")
	fs.StringVar(&c.KeyDir, "key-dir", c.KeyDir, "independent key directory")
	fs.StringVar(&c.SecretDir, "secret-dir", c.SecretDir, "external secret directory")
	fs.StringVar(&c.PublicURL, "public-url", c.PublicURL, "canonical public URL")
	fs.BoolVar(&c.SecureCookie, "secure-cookie", c.SecureCookie, "HTTPS-only cookies")
	fs.BoolVar(&c.AutomaticSync, "automatic-sync", c.AutomaticSync, "automatically reconcile mailbox changes")
	fs.IntVar(&c.SyncIntervalSeconds, "sync-interval-seconds", c.SyncIntervalSeconds, "seconds between completed automatic folder checks (5..3600)")
	fs.BoolVar(&c.MCPEnabled, "mcp-enabled", c.MCPEnabled, "enable authenticated MCP and OAuth at an HTTPS public_url")
	if e := fs.Parse(args); e != nil {
		return c, nil, e
	}
	if _, _, e := net.SplitHostPort(c.Listen); e != nil {
		return c, nil, fmt.Errorf("invalid listen address: %w", e)
	}
	if c.SyncIntervalSeconds < 5 || c.SyncIntervalSeconds > 3600 {
		return c, nil, errors.New("sync_interval_seconds must be 5..3600")
	}
	if c.SessionHours < 1 || c.SessionHours > 168 || c.RetentionDays < 0 || c.RetentionDays > 3650 {
		return c, nil, errors.New("session_hours must be 1..168; retention_days must be 0..3650 (legacy; full history is always retained)")
	}
	if c.PublicURL != "" {
		u, e := url.Parse(c.PublicURL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.User != nil || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return c, nil, errors.New("public_url must be an HTTP(S) origin")
		}
		c.PublicURL = u.Scheme + "://" + u.Host
		if u.Scheme == "https" {
			c.SecureCookie = true
		}
	}
	if c.MCPEnabled && !strings.HasPrefix(c.PublicURL, "https://") {
		return c, nil, errors.New("mcp_enabled requires an HTTPS public_url")
	}
	var e error
	c.DataDir, e = filepath.Abs(c.DataDir)
	if e != nil {
		return c, nil, e
	}
	c.KeyDir, e = filepath.Abs(c.KeyDir)
	if e != nil {
		return c, nil, e
	}
	c.SecretDir, e = filepath.Abs(c.SecretDir)
	if e != nil {
		return c, nil, e
	}
	if c.DataDir == c.KeyDir {
		return c, nil, errors.New("data_dir and key_dir must be separate")
	}
	return c, fs.Args(), nil
}
func (c Config) SessionTTL() time.Duration { return time.Duration(c.SessionHours) * time.Hour }
