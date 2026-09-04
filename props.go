// Package props loads application configuration into plain Go structs.
//
// A configuration is any struct. Its fields hold default values, a YAML file
// and environment variables override them, rules derive further values, and
// Validate methods check the result:
//
//	var cfg Config
//	report, err := props.Load(&cfg, props.File("config.yaml"), props.Env("APP"))
//
// Fields are annotated with the props struct tag:
//
//	required   the user must set the field via file or environment
//	secret     the value is masked in [Report.String]
//	env=NAME   the field always reads environment variable NAME
//	env=-      the field is never read from the environment
//
// YAML keys match field names case-insensitively, or the name given in a yaml
// tag. With [Env], every field also reads PREFIX_PATH_TO_FIELD, the field path
// in SNAKE_CASE, for example APP_SERVICE_DISCOVERY_URL. Environment variables
// override the file.
//
// The configuration struct, and any nested struct, may implement
//
//	Rules() []props.Rule
//
// to declare values computed from other fields with [Derive] and [Default].
// Rules run in the order listed, after all sources are loaded. Any value that
// implements
//
//	Validate() error
//
// is validated after the rules, at any depth, including map values and scalar
// types. Errors are reported as [FieldError] prefixed with the field path.
package props

import (
	"cmp"
	"encoding"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// Source tells where a field's value came from.
type Source int

const (
	SourceDefault Source = iota // the struct's initial value
	SourceFile                  // the YAML file
	SourceEnv                   // an environment variable
	SourceDerived               // a Derive or Default rule
)

func (s Source) String() string {
	names := [...]string{"default", "file", "env", "derived"}
	if uint(s) < uint(len(names)) {
		return names[s]
	}
	return "unknown"
}

// FieldError reports a problem with one field. Path is the dotted field path
// as shown by [Report.String].
type FieldError struct {
	Path string
	Err  error
}

func (e *FieldError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func fieldErr(path, format string, args ...any) error {
	return &FieldError{path, fmt.Errorf(format, args...)}
}

// Option configures [Load].
type Option func(*loader)

// File loads values from the YAML file at path. The file must exist, and every
// key in it must name a field.
func File(path string) Option { return func(l *loader) { l.file = path } }

// OptionalFile is like [File], but a file that does not exist is skipped.
// Any other problem with the file is still an error.
func OptionalFile(path string) Option {
	return func(l *loader) { l.file, l.fileOptional = path, true }
}

// Env loads values from environment variables named PREFIX_PATH_TO_FIELD: the
// field path in SNAKE_CASE, split at word boundaries, with dots and other
// punctuation replaced by underscores. An empty prefix uses the bare field
// path. Two fields reading the same variable is an error.
func Env(prefix string) Option { return func(l *loader) { l.envPrefix = new(prefix) } }

// Rule computes a field's value from other fields. Rules are declared by a
// Rules method on the configuration struct:
//
//	func (c *Config) Rules() []props.Rule {
//		return []props.Rule{
//			props.Derive(&c.Heartbeat, func() time.Duration {
//				if c.DevMode {
//					return time.Second
//				}
//				return time.Minute
//			}),
//		}
//	}
type Rule interface {
	apply(*Report) error
}

type rule[T any] struct {
	ptr      *T
	fn       func() T
	override bool
}

// Derive sets *ptr to fn() after all sources are loaded. The user cannot set
// the field: a value from the file or environment is an error.
func Derive[T any](ptr *T, fn func() T) Rule { return rule[T]{ptr, fn, false} }

// Default sets *ptr to fn() unless the user set the field via file or
// environment. It expresses a default computed from other fields.
func Default[T any](ptr *T, fn func() T) Rule { return rule[T]{ptr, fn, true} }

func (r rule[T]) apply(rep *Report) error {
	path, ok := rep.pathOf(r.ptr)
	if !ok {
		return fmt.Errorf("rule target %T does not point into the configuration", r.ptr)
	}
	if src := rep.sources[path]; src == SourceFile || src == SourceEnv {
		if r.override {
			return nil
		}
		return fieldErr(path, "cannot be set by the user, its value is derived")
	}
	*r.ptr = r.fn()
	rep.sources[path] = SourceDerived
	return nil
}

// Report describes a loaded configuration: where each value came from, and a
// printable form with secrets masked.
type Report struct {
	cfg     reflect.Value
	paths   map[ptrKey]string
	sources map[string]Source
}

// ptrKey includes the type because a struct and its first field share an address.
type ptrKey struct {
	addr uintptr
	typ  reflect.Type
}

func (r *Report) pathOf[T any](ptr *T) (string, bool) {
	path, ok := r.paths[ptrKey{reflect.ValueOf(ptr).Pointer(), reflect.TypeFor[T]()}]
	return path, ok
}

// Source reports where the field that ptr points to got its value. It panics
// if ptr does not point into the loaded configuration.
func (r *Report) Source[T any](ptr *T) Source {
	path, ok := r.pathOf(ptr)
	if !ok {
		panic("props: pointer does not point into the loaded configuration")
	}
	return r.sources[path]
}

// String lists every field with its value and source, one per line, in
// declaration order. Secret values are masked.
func (r *Report) String() string {
	var sb strings.Builder
	sb.WriteString(cmp.Or(r.cfg.Type().Elem().Name(), "config") + " {\n")
	walk("", r.cfg, tags{}, func(path string, t tags, v reflect.Value) error {
		if path != "" && isLeaf(v) {
			fmt.Fprintf(&sb, "  %s: %s (%s)\n", path, format(v, t.secret), r.sources[path])
		}
		return nil
	})
	sb.WriteString("}")
	return sb.String()
}

func format(v reflect.Value, secret bool) string {
	if secret {
		return "********"
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "<nil>"
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.String {
		return strconv.Quote(v.String())
	}
	return fmt.Sprint(v.Interface())
}

type loader struct {
	Report
	file         string
	fileOptional bool
	envPrefix    *string
}

// Load fills the struct cfg from the given sources, applies its rules and
// validates it. Environment variables override the file, which overrides the
// initial field values.
func Load[T any](cfg *T, opts ...Option) (*Report, error) {
	v := reflect.ValueOf(cfg)
	if v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("props: cfg must be a non-nil pointer to a struct, got %T", cfg)
	}
	l := &loader{cfg: v, paths: map[ptrKey]string{}, sources: map[string]Source{}}
	for _, opt := range opts {
		opt(l)
	}
	if err := l.load(); err != nil {
		return nil, fmt.Errorf("props: %w", err)
	}
	return &l.Report, nil
}

func (l *loader) load() error {
	if l.file != "" {
		if err := l.loadFile(); err != nil {
			return err
		}
	}
	if err := l.loadEnv(); err != nil {
		return err
	}
	if err := l.checkRequired(); err != nil {
		return err
	}
	if err := l.applyRules(); err != nil {
		return err
	}
	return l.validate()
}

func (l *loader) loadFile() error {
	data, err := os.ReadFile(l.file)
	if l.fileOptional && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", l.file, err)
	}
	if doc.Kind == 0 {
		return nil
	}
	if err := l.matchKeys(&doc, l.cfg.Type(), ""); err != nil {
		return fmt.Errorf("%s: %w", l.file, err)
	}
	if err := doc.Decode(l.cfg.Interface()); err != nil {
		return fmt.Errorf("%s: %w", l.file, err)
	}
	return nil
}

// matchKeys rewrites the mapping keys under n to the names yaml.v3 expects for
// type t, matching field names case-insensitively. It records every key as
// SourceFile and rejects keys that do not name a field.
func (l *loader) matchKeys(n *yaml.Node, t reflect.Type, path string) error {
	switch n.Kind {
	case yaml.DocumentNode:
		return l.matchKeys(n.Content[0], t, path)
	case yaml.AliasNode:
		return l.matchKeys(n.Alias, t, path)
	}
	if path != "" {
		l.sources[path] = SourceFile
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case isStruct(t) && n.Kind == yaml.MappingNode:
		fields := yamlFields(t, map[string]yamlField{})
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			f, ok := fields[strings.ToLower(key.Value)]
			if !ok {
				return fieldErr(join(path, key.Value), "unknown key")
			}
			key.Value = f.key
			if err := l.matchKeys(val, f.typ, join(path, f.seg)); err != nil {
				return err
			}
		}
	case t.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := l.matchKeys(n.Content[i+1], t.Elem(), join(path, n.Content[i].Value)); err != nil {
				return err
			}
		}
	case t.Kind() == reflect.Slice && n.Kind == yaml.SequenceNode:
		for i, c := range n.Content {
			if err := l.matchKeys(c, t.Elem(), join(path, strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

type yamlField struct {
	key, seg string // key as yaml.v3 expects it, seg as used in field paths
	typ      reflect.Type
}

// yamlFields indexes the exported fields of struct type t by lower-cased YAML
// key, flattening inlined fields into their parent.
func yamlFields(t reflect.Type, into map[string]yamlField) map[string]yamlField {
	for sf := range t.Fields() {
		key, seg, inline := yamlName(sf)
		switch {
		case !sf.IsExported() || key == "":
		case inline && sf.Type.Kind() == reflect.Struct:
			yamlFields(sf.Type, into)
		default:
			into[strings.ToLower(key)] = yamlField{key, seg, sf.Type}
		}
	}
	return into
}

func (l *loader) loadEnv() error {
	bound := map[string]string{} // variable name to the field path reading it
	return walk("", l.cfg, tags{}, func(path string, t tags, v reflect.Value) error {
		name := t.env
		switch {
		case name == "-":
			return nil
		case name == "":
			if l.envPrefix == nil || path == "" || !isLeaf(v) {
				return nil
			}
			name = envName(path)
			if *l.envPrefix != "" {
				name = *l.envPrefix + "_" + name
			}
		}
		if other, ok := bound[name]; ok {
			return fieldErr(path, "environment variable %s is already read by %s", name, other)
		}
		bound[name] = path
		s, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		if !v.CanSet() {
			return fieldErr(path, "cannot be set from environment variable %s", name)
		}
		if err := parse(v, s); err != nil {
			return fieldErr(path, "environment variable %s: %w", name, err)
		}
		l.sources[path] = SourceEnv
		return nil
	})
}

func (l *loader) checkRequired() error {
	var errs []error
	err := walk("", l.cfg, tags{}, func(path string, t tags, _ reflect.Value) error {
		if t.required && l.sources[path] == SourceDefault {
			errs = append(errs, fieldErr(path, "required"))
		}
		return nil
	})
	return errors.Join(append(errs, err)...)
}

// applyRules indexes the field paths, collects the rules of every struct in
// walk order and applies them. It runs after the sources are loaded so that
// pointer fields allocated by the file are indexed too.
func (l *loader) applyRules() error {
	var rules []Rule
	var copies []mapCopy
	var collect func(path string, _ tags, v reflect.Value) error
	collect = func(path string, _ tags, v reflect.Value) error {
		if v.CanAddr() {
			l.paths[ptrKey{v.Addr().Pointer(), v.Type()}] = path
		}
		if v = deref(v); !v.IsValid() {
			return nil
		}
		switch t := v.Type(); {
		case t.Kind() == reflect.Map && t.Elem().Kind() == reflect.Struct && isStruct(t.Elem()):
			// Map values are not addressable, so their rules point into copies
			// that are written back once all rules ran.
			for _, k := range sortedKeys(v) {
				e := reflect.New(t.Elem()).Elem()
				e.Set(v.MapIndex(k))
				if err := walk(join(path, fmt.Sprint(k)), e, tags{}, collect); err != nil {
					return err
				}
				copies = append(copies, mapCopy{v, k, e})
			}
			return errSkip
		case v.CanAddr():
			if r, ok := v.Addr().Interface().(interface{ Rules() []Rule }); ok {
				rules = append(rules, r.Rules()...)
			}
		}
		return nil
	}
	if err := walk("", l.cfg, tags{}, collect); err != nil {
		return err
	}
	for _, r := range rules {
		if err := r.apply(&l.Report); err != nil {
			return err
		}
	}
	for _, c := range copies {
		c.m.SetMapIndex(c.k, c.v)
	}
	// The copies are garbage now: forget their addresses so that a later
	// allocation at the same address cannot be mistaken for a field.
	maps.DeleteFunc(l.paths, func(k ptrKey, _ string) bool {
		return slices.ContainsFunc(copies, func(c mapCopy) bool { return c.contains(k.addr) })
	})
	return nil
}

// mapCopy is an addressable copy v of the value of m at key k.
type mapCopy struct{ m, k, v reflect.Value }

func (c mapCopy) contains(addr uintptr) bool {
	start := c.v.UnsafeAddr()
	return addr >= start && addr < start+max(c.v.Type().Size(), 1)
}

func (l *loader) validate() error {
	var errs []error
	err := walk("", l.cfg, tags{}, func(path string, _ tags, v reflect.Value) error {
		v = deref(v)
		if !v.IsValid() {
			return nil
		}
		if !v.CanAddr() { // map values: validate an addressable copy
			p := reflect.New(v.Type())
			p.Elem().Set(v)
			v = p.Elem()
		}
		val, ok := v.Addr().Interface().(interface{ Validate() error })
		if !ok {
			return nil
		}
		switch err := val.Validate(); {
		case err == nil:
		case path == "":
			errs = append(errs, err)
		default:
			errs = append(errs, &FieldError{path, err})
		}
		return nil
	})
	return errors.Join(append(errs, err)...)
}

type tags struct {
	required, secret bool
	env              string // variable name, or "-" for none
}

// inheritable returns the tags a value passes on to its children: secrecy and
// exclusion from the environment.
func (t tags) inheritable() tags {
	var c tags
	c.secret = t.secret
	if t.env == "-" {
		c.env = "-"
	}
	return c
}

// inherit combines the field's own tags with those of its parent.
func (t tags) inherit(parent tags) tags {
	p := parent.inheritable()
	t.secret = t.secret || p.secret
	if t.env == "" {
		t.env = p.env
	}
	return t
}

func parseTags(sf reflect.StructField) (tags, error) {
	var t tags
	for part := range strings.SplitSeq(sf.Tag.Get("props"), ",") {
		switch name, ok := strings.CutPrefix(strings.TrimSpace(part), "env="); {
		case ok:
			t.env = name
		case part == "required":
			t.required = true
		case part == "secret":
			t.secret = true
		case part != "":
			return t, fmt.Errorf("unknown props tag %q", part)
		}
	}
	return t, nil
}

// yamlName returns the key yaml.v3 expects for sf (empty when the field is
// excluded with "-"), the segment props uses in field paths, and whether the
// field is inlined into its parent.
func yamlName(sf reflect.StructField) (key, seg string, inline bool) {
	name, opts, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
	inline = slices.Contains(strings.Split(opts, ","), "inline")
	switch name {
	case "":
		return strings.ToLower(sf.Name), sf.Name, inline
	case "-":
		return "", sf.Name, inline
	}
	return name, name, inline
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}

// envName converts a field path to SNAKE_CASE: a word starts at a dot or other
// punctuation, at an upper-case letter after a lower-case letter or digit, and
// at the last of two or more upper-case letters when a lower-case one follows.
// So ServiceDiscovery.HTTPClient.APIKey becomes SERVICE_DISCOVERY_HTTP_CLIENT_API_KEY,
// while a single capital as in OAuth or IPv6 does not split.
func envName(path string) string {
	var sb strings.Builder
	runes := []rune(path)
	upperAt := func(i int) bool { return i >= 0 && unicode.IsUpper(runes[i]) }
	lowerAt := func(i int) bool { return i < len(runes) && unicode.IsLower(runes[i]) }
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			sb.WriteByte('_')
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (upperAt(i-1) && upperAt(i-2) && lowerAt(i+1)) {
				sb.WriteByte('_')
			}
		}
		sb.WriteRune(unicode.ToUpper(r))
	}
	return sb.String()
}

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// isStruct reports whether t (or the type it points to) is a struct that props
// walks into, as opposed to one parsed from text such as time.Time.
func isStruct(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && !reflect.PointerTo(t).Implements(textUnmarshaler)
}

// isLeaf reports whether v is shown as a single value rather than walked into.
func isLeaf(v reflect.Value) bool {
	if v = deref(v); !v.IsValid() {
		return true
	}
	switch t := v.Type(); t.Kind() {
	case reflect.Map, reflect.Slice:
		return v.Len() == 0 || !isStruct(t.Elem())
	default:
		return !isStruct(t)
	}
}

// deref follows a pointer, returning the zero Value for nil.
func deref(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		return v.Elem()
	}
	return v
}

// errSkip is returned by a visit function to keep walk out of the value's children.
var errSkip = errors.New("skip")

// walk visits v and then, depth first and in declaration order, every exported
// field, map value and slice element that is a struct. Inlined fields keep
// their parent's path, and a secret value's children are secret too.
func walk(path string, v reflect.Value, t tags, visit func(string, tags, reflect.Value) error) error {
	if err := visit(path, t, v); err != nil {
		if err == errSkip {
			return nil
		}
		return err
	}
	if v = deref(v); !v.IsValid() {
		return nil
	}
	inherited := t.inheritable()
	switch vt := v.Type(); {
	case isStruct(vt):
		for sf, fv := range v.Fields() {
			if !sf.IsExported() {
				continue
			}
			_, seg, inline := yamlName(sf)
			p := path
			if !inline {
				p = join(path, seg)
			}
			ft, err := parseTags(sf)
			if err != nil {
				return &FieldError{p, err}
			}
			ft = ft.inherit(t)
			if err := walk(p, fv, ft, visit); err != nil {
				return err
			}
		}
	case vt.Kind() == reflect.Map && isStruct(vt.Elem()):
		for _, k := range sortedKeys(v) {
			if err := walk(join(path, fmt.Sprint(k)), v.MapIndex(k), inherited, visit); err != nil {
				return err
			}
		}
	case vt.Kind() == reflect.Slice && isStruct(vt.Elem()):
		for i := range v.Len() {
			if err := walk(join(path, strconv.Itoa(i)), v.Index(i), inherited, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortedKeys returns the keys of map v ordered by their printed form.
func sortedKeys(v reflect.Value) []reflect.Value {
	return slices.SortedFunc(slices.Values(v.MapKeys()), func(a, b reflect.Value) int {
		return cmp.Compare(fmt.Sprint(a), fmt.Sprint(b))
	})
}

var durationType = reflect.TypeFor[time.Duration]()

// parse sets v from its textual form as found in an environment variable.
// Slices are comma separated; empty items are dropped.
func parse(v reflect.Value, s string) error {
	if v.CanAddr() {
		if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return u.UnmarshalText([]byte(s))
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return parse(v.Elem(), s)
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Type() == durationType {
			d, err := time.ParseDuration(s)
			if err != nil {
				return err
			}
			v.SetInt(int64(d))
			return nil
		}
		i, err := strconv.ParseInt(s, 0, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(s, 0, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.Slice:
		var parts []string
		for p := range strings.SplitSeq(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		out := reflect.MakeSlice(v.Type(), len(parts), len(parts))
		for i, p := range parts {
			if err := parse(out.Index(i), p); err != nil {
				return err
			}
		}
		v.Set(out)
	default:
		return fmt.Errorf("cannot parse %s from text", v.Type())
	}
	return nil
}
