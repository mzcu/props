package props_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mzcu/props"
)

// The types below model the example use case from CLAUDE.md.

type Environment string

const (
	EnvDev     Environment = "dev"
	EnvStaging Environment = "staging"
	EnvProd    Environment = "prod"
)

func (e Environment) Validate() error {
	if envs := []Environment{EnvDev, EnvStaging, EnvProd}; !slices.Contains(envs, e) {
		return fmt.Errorf("must be one of %v, got %q", envs, e)
	}
	return nil
}

type RetryConfig struct {
	Timeout time.Duration
	Retries int
}

type Endpoint struct {
	URL         string `props:"required"`
	Token       string `props:"secret"`
	RetryConfig `yaml:",inline"`
}

func (e Endpoint) Validate() error {
	if e.Retries < 0 {
		return errors.New("retries must not be negative")
	}
	return nil
}

type ServiceDiscovery struct {
	URL               string
	Password          string `props:"secret"`
	HeartbeatInterval time.Duration
}

type Config struct {
	Environment      Environment `props:"required"`
	DevMode          bool
	ServiceDiscovery ServiceDiscovery
	Endpoints        map[string]Endpoint
	IgnoredPeers     []string
}

func (c *Config) Rules() []props.Rule {
	return []props.Rule{
		props.Default(&c.ServiceDiscovery.URL, func() string {
			return map[Environment]string{
				EnvDev:     "http://localhost:8500",
				EnvStaging: "http://consul.staging.internal:8500",
				EnvProd:    "http://consul.prod.internal:8500",
			}[c.Environment]
		}),
		props.Derive(&c.ServiceDiscovery.HeartbeatInterval, func() time.Duration {
			if c.DevMode {
				return time.Second
			}
			return time.Minute
		}),
	}
}

func (c *Config) Validate() error {
	if c.Environment == EnvProd && c.DevMode {
		return errors.New("devMode cannot be enabled in production")
	}
	return nil
}

const fullYAML = `
environment: staging
devMode: true
serviceDiscovery:
  password: secret123
ignoredPeers: [peer1.example.com, peer2.example.com]
endpoints:
  api:
    url: http://api.example.com
    token: tok123
    timeout: 30s
    retries: 3
  auth:
    url: http://auth.example.com
    timeout: 10s
`

func TestIntegration_LoadFromYAML(t *testing.T) {
	var cfg Config
	report, err := props.Load(&cfg, props.File(writeYAML(t, fullYAML)))
	if err != nil {
		t.Fatal(err)
	}

	want := Config{
		Environment: EnvStaging,
		DevMode:     true,
		ServiceDiscovery: ServiceDiscovery{
			URL:               "http://consul.staging.internal:8500",
			Password:          "secret123",
			HeartbeatInterval: time.Second,
		},
		Endpoints: map[string]Endpoint{
			"api":  {URL: "http://api.example.com", Token: "tok123", RetryConfig: RetryConfig{30 * time.Second, 3}},
			"auth": {URL: "http://auth.example.com", RetryConfig: RetryConfig{Timeout: 10 * time.Second}},
		},
		IgnoredPeers: []string{"peer1.example.com", "peer2.example.com"},
	}
	if fmt.Sprint(cfg) != fmt.Sprint(want) {
		t.Errorf("loaded config\n got: %+v\nwant: %+v", cfg, want)
	}

	if got := report.Source(&cfg.Environment); got != props.SourceFile {
		t.Errorf("Source(Environment) = %v, want file", got)
	}
	if got := report.Source(&cfg.ServiceDiscovery.HeartbeatInterval); got != props.SourceDerived {
		t.Errorf("Source(HeartbeatInterval) = %v, want derived", got)
	}
}

func TestIntegration_EnvOverridesFile(t *testing.T) {
	t.Setenv("APP_ENVIRONMENT", "dev")
	t.Setenv("APP_SERVICEDISCOVERY_PASSWORD", "envpassword")
	t.Setenv("APP_DEVMODE", "false")
	t.Setenv("APP_IGNOREDPEERS", "peer3.example.com, peer4.example.com")

	var cfg Config
	report, err := props.Load(&cfg, props.File(writeYAML(t, fullYAML)), props.Env("APP"))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Environment != EnvDev || cfg.ServiceDiscovery.Password != "envpassword" || cfg.DevMode {
		t.Errorf("env vars did not override file: %+v", cfg)
	}
	if want := []string{"peer3.example.com", "peer4.example.com"}; !slices.Equal(cfg.IgnoredPeers, want) {
		t.Errorf("IgnoredPeers = %v, want %v", cfg.IgnoredPeers, want)
	}
	if cfg.ServiceDiscovery.HeartbeatInterval != time.Minute {
		t.Errorf("HeartbeatInterval = %v, want 1m after devMode was disabled by env", cfg.ServiceDiscovery.HeartbeatInterval)
	}
	if got := report.Source(&cfg.Environment); got != props.SourceEnv {
		t.Errorf("Source(Environment) = %v, want env", got)
	}
}

func TestIntegration_DefaultRuleKeepsUserValue(t *testing.T) {
	yaml := `
environment: staging
serviceDiscovery:
  url: http://custom.consul.internal:8500
`
	var cfg Config
	report, err := props.Load(&cfg, props.File(writeYAML(t, yaml)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceDiscovery.URL != "http://custom.consul.internal:8500" {
		t.Errorf("URL = %q, want the user-provided value", cfg.ServiceDiscovery.URL)
	}
	if got := report.Source(&cfg.ServiceDiscovery.URL); got != props.SourceFile {
		t.Errorf("Source(URL) = %v, want file", got)
	}
}

func TestIntegration_DerivedFieldCannotBeSet(t *testing.T) {
	t.Setenv("APP_SERVICEDISCOVERY_HEARTBEATINTERVAL", "5s")
	t.Setenv("APP_ENVIRONMENT", "dev")

	var cfg Config
	_, err := props.Load(&cfg, props.Env("APP"))
	assertFieldError(t, err, "ServiceDiscovery.HeartbeatInterval", "derived")
}

func TestIntegration_ValidationErrors(t *testing.T) {
	tests := []struct {
		name, yaml, path, msg string
	}{
		{"missing required", "devMode: true", "Environment", "required"},
		{"invalid environment", "environment: local", "Environment", "must be one of"},
		{"devMode in prod", "environment: prod\ndevMode: true", "", "devMode cannot be enabled in production"},
		{"required inside map", "environment: dev\nendpoints:\n  api: {timeout: 1s}", "Endpoints.api.URL", "required"},
		{"validate inside map", "environment: dev\nendpoints:\n  api: {url: http://x, retries: -1}", "Endpoints.api", "retries must not be negative"},
		{"unknown key", "environment: dev\nserviceDiscovery:\n  passwrd: x", "ServiceDiscovery.passwrd", "unknown key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			_, err := props.Load(&cfg, props.File(writeYAML(t, tt.yaml)))
			assertFieldError(t, err, tt.path, tt.msg)
		})
	}
}

func TestIntegration_MissingFile(t *testing.T) {
	var cfg Config
	_, err := props.Load(&cfg, props.File(filepath.Join(t.TempDir(), "missing.yaml")))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestIntegration_ReportString(t *testing.T) {
	var cfg Config
	report, err := props.Load(&cfg, props.File(writeYAML(t, fullYAML)))
	if err != nil {
		t.Fatal(err)
	}

	want := `Config {
  Environment: "staging" (file)
  DevMode: true (file)
  ServiceDiscovery.URL: "http://consul.staging.internal:8500" (derived)
  ServiceDiscovery.Password: ******** (file)
  ServiceDiscovery.HeartbeatInterval: 1s (derived)
  Endpoints.api.URL: "http://api.example.com" (file)
  Endpoints.api.Token: ******** (file)
  Endpoints.api.Timeout: 30s (file)
  Endpoints.api.Retries: 3 (file)
  Endpoints.auth.URL: "http://auth.example.com" (file)
  Endpoints.auth.Token: ******** (default)
  Endpoints.auth.Timeout: 10s (file)
  Endpoints.auth.Retries: 0 (default)
  IgnoredPeers: [peer1.example.com peer2.example.com] (file)
}`
	if got := report.String(); got != want {
		t.Errorf("String()\n got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(report.String(), "secret123") || strings.Contains(report.String(), "tok123") {
		t.Error("secrets leaked into the report")
	}
}

func assertFieldError(t *testing.T, err error, path, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %q", msg)
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("err = %q, want it to contain %q", err, msg)
	}
	if path == "" {
		return
	}
	fe, ok := errors.AsType[*props.FieldError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *props.FieldError", err, err)
	}
	if fe.Path != path {
		t.Errorf("FieldError.Path = %q, want %q", fe.Path, path)
	}
}

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
