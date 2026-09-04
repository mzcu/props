# props

A minimal Go library for loading application configuration into plain structs from
YAML files and environment variables, with derived values, validation, secret masking
and a printable report of where every value came from.

The only dependency beyond the standard library is `go.yaml.in/yaml/v3`.

## Install

```bash
go get github.com/mzcu/props
```

## Quick start

```go
package main

import (
    "errors"
    "log"
    "time"

    "github.com/mzcu/props"
)

type Config struct {
    Environment string        `props:"required"`
    Port        int
    Timeout     time.Duration
    APIKey      string        `props:"secret"`
    DevMode     bool
}

func (c *Config) Validate() error {
    if c.Port <= 0 || c.Port > 65535 {
        return errors.New("port must be between 1 and 65535")
    }
    return nil
}

func main() {
    cfg := Config{Port: 8080, Timeout: time.Minute} // defaults

    report, err := props.Load(&cfg, props.File("config.yaml"), props.Env("MYAPP"))
    if err != nil {
        log.Fatal(err)
    }
    log.Println(report) // every field, its value and source; secrets masked
}
```

`Load` fills the struct in this order:

1. Initial field values are the defaults.
2. The YAML file overrides them.
3. Environment variables override the file.
4. `required` fields must have been set by step 2 or 3.
5. Rules run, in the order listed.
6. `Validate()` methods run.

## Struct tags

| Tag | Meaning |
|-----|---------|
| `props:"required"` | The user must set the field via file or environment |
| `props:"secret"` | The value is masked in the report |
| `props:"env=NAME"` | The field always reads environment variable `NAME` |
| `yaml:"name"` | The YAML key and report name, as in `yaml.v3` |

YAML keys match field names case-insensitively, so `serviceDiscovery`, `servicediscovery`
and `ServiceDiscovery` all match a field named `ServiceDiscovery`. Unknown keys and a
missing file are errors.

## Environment variables

Nothing is read from the environment unless you ask for it. With `props.Env("MYAPP")`,
every field reads `MYAPP_` followed by its path in upper case with dots replaced by
underscores, for example `MYAPP_SERVICEDISCOVERY_URL`. `props.Env("")` uses the bare
path. Fields tagged `env=NAME` read `NAME` regardless.

Values are parsed by type: strings, booleans, integers, floats, `time.Duration`, any
type implementing `encoding.TextUnmarshaler` such as `time.Time` and `netip.Addr`, and
comma-separated slices of those. Map entries cannot be set from the environment.

## Derived values

Implement `Rules()` on the config struct, or on any nested struct, to compute fields from
other fields. Rules run in the order listed, after all sources are loaded, so a rule may
read the result of an earlier one.

```go
func (c *Config) Rules() []props.Rule {
    return []props.Rule{
        // A computed default: the user may override it via file or environment.
        props.Default(&c.ServiceDiscovery.URL, func() string {
            return "https://" + c.Environment + ".example.com/discovery"
        }),
        // Always computed: a user-provided value is an error.
        props.Derive(&c.ServiceDiscovery.HeartbeatInterval, func() time.Duration {
            if c.DevMode {
                return time.Second
            }
            return time.Minute
        }),
    }
}
```

## Validation

Any value implementing `Validate() error` is validated after the rules ran: the config
struct, nested structs, map values, slice elements and scalar types alike. Errors are
prefixed with the field path.

```go
type Environment string

func (e Environment) Validate() error {
    if !slices.Contains([]Environment{"dev", "staging", "prod"}, e) {
        return fmt.Errorf("must be one of dev, staging, prod, got %q", e)
    }
    return nil
}

func (c *Config) Validate() error {
    if c.Environment == "prod" && c.DevMode {
        return errors.New("devMode cannot be enabled in production")
    }
    return nil
}
```

Problems with individual fields are reported as `*props.FieldError` carrying the path,
and several problems are joined into one error:

```
props: Environment: must be one of dev, staging, prod, got "local"
Endpoints.api.URL: required
```

## Report

`Load` returns a `*props.Report`. Its `String()` lists every field in declaration order
with its value and source, one of `default`, `file`, `env` or `derived`. Secrets are
masked, including inside maps and slices.

```
Config {
  Environment: "staging" (file)
  DevMode: true (file)
  ServiceDiscovery.URL: "http://consul.staging.internal:8500" (derived)
  ServiceDiscovery.Password: ******** (file)
  ServiceDiscovery.HeartbeatInterval: 1s (derived)
  Endpoints.api.URL: "http://api.example.com" (file)
  Endpoints.api.Timeout: 30s (file)
}
```

`report.Source(&cfg.Field)` returns the source of a single field.

## Complete example

See [props_integration_test.go](props_integration_test.go) for a configuration with
nested structs, a map of endpoints, secrets, derived values and validation.

## License

MIT
