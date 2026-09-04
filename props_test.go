package props_test

import (
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mzcu/props"
)

func TestLoad_YAMLKeysMatchCaseInsensitively(t *testing.T) {
	var cfg struct {
		DevMode bool   `yaml:"devMode"`
		Name    string `yaml:"displayName"`
		Nested  struct{ MaxRetries int }
	}
	yaml := "DEVMODE: true\ndisplayname: app\nnested: {maxretries: 3}"
	report, err := props.Load(&cfg, props.File(writeYAML(t, yaml)))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DevMode || cfg.Name != "app" || cfg.Nested.MaxRetries != 3 {
		t.Errorf("cfg = %+v", cfg)
	}
	if got := report.Source(&cfg.DevMode); got != props.SourceFile {
		t.Errorf("Source(DevMode) = %v, want file for a tagged field", got)
	}
	if !strings.Contains(report.String(), "devMode: true (file)") {
		t.Errorf("String() should use the yaml tag name:\n%s", report)
	}
}

func TestLoad_EnvIsOptIn(t *testing.T) {
	t.Setenv("NAME", "from-env")
	t.Setenv("APP_NAME", "from-prefixed-env")
	t.Setenv("POD_IP", "10.0.0.42")

	var cfg struct {
		Name  string
		PodIP string `props:"env=POD_IP"`
	}
	if _, err := props.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "" || cfg.PodIP != "10.0.0.42" {
		t.Errorf("without Env only env= tags apply, got %+v", cfg)
	}

	if _, err := props.Load(&cfg, props.Env("APP")); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from-prefixed-env" {
		t.Errorf("Name = %q, want the APP_ prefixed value", cfg.Name)
	}

	if _, err := props.Load(&cfg, props.Env("")); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from-env" {
		t.Errorf("Name = %q, want the unprefixed value", cfg.Name)
	}
}

func TestLoad_EnvParseError(t *testing.T) {
	t.Setenv("APP_PORT", "http")
	var cfg struct{ Port int }
	_, err := props.Load(&cfg, props.Env("APP"))
	assertFieldError(t, err, "Port", "APP_PORT")
}

type chain struct {
	Base   string
	Middle string
	Top    string
}

func (c *chain) Rules() []props.Rule {
	return []props.Rule{
		props.Derive(&c.Middle, func() (string, error) { return c.Base + "-middle", nil }),
		props.Derive(&c.Top, func() (string, error) { return c.Middle + "-top", nil }),
	}
}

func TestLoad_RulesRunInDeclarationOrder(t *testing.T) {
	cfg := chain{Base: "base"}
	if _, err := props.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Top != "base-middle-top" {
		t.Errorf("Top = %q, want base-middle-top", cfg.Top)
	}
}

type outer struct {
	Inner inner
}

type inner struct {
	Host string
	Addr string
}

func (i *inner) Rules() []props.Rule {
	return []props.Rule{props.Derive(&i.Addr, func() (string, error) { return i.Host + ":80", nil })}
}

func TestLoad_NestedStructRules(t *testing.T) {
	cfg := outer{Inner: inner{Host: "example.com"}}
	if _, err := props.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Inner.Addr != "example.com:80" {
		t.Errorf("Inner.Addr = %q", cfg.Inner.Addr)
	}
}

type stray struct{ Name string }

func (s *stray) Rules() []props.Rule {
	var elsewhere string
	return []props.Rule{props.Derive(&elsewhere, func() (string, error) { return "", nil })}
}

func TestLoad_RuleTargetOutsideConfig(t *testing.T) {
	_, err := props.Load(&stray{})
	if err == nil || !strings.Contains(err.Error(), "does not point into the configuration") {
		t.Errorf("err = %v", err)
	}
}

func TestLoad_PointerAndTextFields(t *testing.T) {
	t.Setenv("APP_LIMIT", "7")
	t.Setenv("APP_SINCE", "2024-05-01T00:00:00Z")
	var cfg struct {
		Limit *int
		Since time.Time
		Rate  *float64
	}
	report, err := props.Load(&cfg, props.Env("APP"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limit == nil || *cfg.Limit != 7 || cfg.Since.Year() != 2024 {
		t.Errorf("cfg = %+v", cfg)
	}
	want := "  Limit: 7 (env)\n  Since: 2024-05-01 00:00:00 +0000 UTC (env)\n  Rate: <nil> (default)\n"
	if got := report.String(); !strings.Contains(got, want) {
		t.Errorf("String() =\n%s\nwant to contain\n%s", got, want)
	}
}

func TestLoad_Errors(t *testing.T) {
	var notStruct int
	if _, err := props.Load(&notStruct); err == nil {
		t.Error("expected an error for a non-struct")
	}
	var nilCfg *struct{ Name string }
	if _, err := props.Load(nilCfg); err == nil {
		t.Error("expected an error for a nil pointer")
	}

	var badTag struct {
		Name string `props:"requried"`
	}
	_, err := props.Load(&badTag)
	assertFieldError(t, err, "Name", `unknown props tag "requried"`)
}

func TestLoad_MultipleErrorsAreJoined(t *testing.T) {
	var cfg struct {
		A string `props:"required"`
		B string `props:"required"`
	}
	_, err := props.Load(&cfg)
	var count int
	for _, e := range []string{"A", "B"} {
		if strings.Contains(err.Error(), e+": required") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("err = %q, want both fields reported", err)
	}
	if _, ok := errors.AsType[*props.FieldError](err); !ok {
		t.Errorf("err = %T, want to unwrap to *props.FieldError", err)
	}
}

func TestReport_SourcePanicsForForeignPointer(t *testing.T) {
	var cfg struct{ Name string }
	report, err := props.Load(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("expected a panic")
		}
	}()
	var other string
	report.Source(&other)
}

type credentials struct{ User, Password string }

func TestReport_SecretMasksNestedValues(t *testing.T) {
	cfg := struct {
		DB     credentials            `props:"secret"`
		Tokens map[string]credentials `props:"secret"`
		Public credentials
	}{
		DB:     credentials{"root", "hunter2"},
		Tokens: map[string]credentials{"api": {"svc", "hunter3"}},
		Public: credentials{"guest", "visible"},
	}
	report, err := props.Load(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := report.String()
	for _, s := range []string{"hunter2", "hunter3", "root", "svc"} {
		if strings.Contains(got, s) {
			t.Errorf("String() exposes %q:\n%s", s, got)
		}
	}
	if !strings.Contains(got, `Public.Password: "visible"`) {
		t.Errorf("String() should not mask untagged fields:\n%s", got)
	}
}

type endpoint struct{ Host, Addr string }

func (e *endpoint) Rules() []props.Rule {
	return []props.Rule{props.Derive(&e.Addr, func() (string, error) { return e.Host + ":443", nil })}
}

func TestLoad_RulesApplyToMapValues(t *testing.T) {
	var cfg struct{ Endpoints map[string]endpoint }
	report, err := props.Load(&cfg, props.File(writeYAML(t, "endpoints: {api: {host: api.example.com}}")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Endpoints["api"].Addr; got != "api.example.com:443" {
		t.Errorf("Endpoints[api].Addr = %q", got)
	}
	if !strings.Contains(report.String(), `Endpoints.api.Addr: "api.example.com:443" (derived)`) {
		t.Errorf("String() =\n%s", report)
	}

	_, err = props.Load(&cfg, props.File(writeYAML(t, "endpoints: {api: {host: h, addr: user}}")))
	assertFieldError(t, err, "Endpoints.api.Addr", "cannot be set by the user")
}

func TestLoad_EnvNamesMarkNestingOnly(t *testing.T) {
	t.Setenv("APP_OTEL_ENDPOINT", "nested")
	t.Setenv("APP_OTELENDPOINT", "flat")
	t.Setenv("APP_HTTPCLIENT_APIKEY", "key")
	var cfg struct {
		OtelEndpoint string
		Otel         struct{ Endpoint string }
		HTTPClient   struct{ APIKey string }
	}
	if _, err := props.Load(&cfg, props.Env("APP")); err != nil {
		t.Fatal(err)
	}
	if cfg.OtelEndpoint != "flat" || cfg.Otel.Endpoint != "nested" || cfg.HTTPClient.APIKey != "key" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoad_EnvNameCollision(t *testing.T) {
	var cfg struct {
		Otel_Port string
		Otel      struct{ Port string }
	}
	_, err := props.Load(&cfg, props.Env("APP"))
	assertFieldError(t, err, "Otel.Port", "APP_OTEL_PORT is already read by Otel_Port")
}

func TestLoad_EnvOptOut(t *testing.T) {
	t.Setenv("APP_EXTRA", "k=v")
	t.Setenv("APP_INTERNAL_NAME", "x")
	var cfg struct {
		Extra    any                   `props:"env=-"`
		Internal struct{ Name string } `props:"env=-"`
	}
	if _, err := props.Load(&cfg, props.Env("APP")); err != nil {
		t.Fatal(err)
	}
	if cfg.Extra != nil || cfg.Internal.Name != "" {
		t.Errorf("cfg = %+v, want nothing read from the environment", cfg)
	}
}

func TestLoad_OptionalFile(t *testing.T) {
	t.Setenv("APP_NAME", "from-env")
	var cfg struct{ Name string }
	dir := t.TempDir()
	if _, err := props.Load(&cfg, props.OptionalFile(filepath.Join(dir, "missing.yaml")), props.Env("APP")); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from-env" {
		t.Errorf("Name = %q, want the env value after skipping a missing file", cfg.Name)
	}
	if _, err := props.Load(&cfg, props.OptionalFile(writeYAML(t, "name: from-file"))); err != nil || cfg.Name != "from-file" {
		t.Errorf("Name = %q, err = %v, want the file to be loaded when present", cfg.Name, err)
	}
	if _, err := props.Load(&cfg, props.OptionalFile(dir)); err == nil {
		t.Error("expected an error for a directory")
	}
}

func TestLoad_SkippedField(t *testing.T) {
	t.Setenv("APP_RUNTIME", "from-env")
	var cfg struct {
		Name    string
		Runtime string `props:"-"`
	}
	report, err := props.Load(&cfg, props.File(writeYAML(t, "name: x")), props.Env("APP"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime != "" || strings.Contains(report.String(), "Runtime") {
		t.Errorf("a skipped field was loaded or reported: cfg = %+v\n%s", cfg, report)
	}
	_, err = props.Load(&cfg, props.File(writeYAML(t, "runtime: x")))
	assertFieldError(t, err, "runtime", "unknown key")
}

type listener struct {
	Host string
	Port int
	URL  *url.URL
	Seen bool
}

func (l *listener) Rules() []props.Rule {
	return []props.Rule{
		props.Derive(&l.URL, func() (*url.URL, error) {
			return url.Parse("http://" + l.Host + ":" + strconv.Itoa(l.Port))
		}),
		props.Derive(&l.Seen, func() (bool, error) { return true, nil }),
	}
}

func TestLoad_RuleError(t *testing.T) {
	cfg := listener{Host: "bad host", Port: 80}
	_, err := props.Load(&cfg)
	assertFieldError(t, err, "URL", "invalid character")
	if cfg.Seen {
		t.Error("rules after a failing one should not run")
	}
}
