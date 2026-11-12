package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// config is the JSON document the program reads. Every key is the Go field name of the kit type
// (or of the types below), matched exactly; an unknown key is an error. A field added later to
// Options, StoreOptions, PeerOptions, Faults or StartTLSOptions can be used from JSON at once,
// because the decoder walks those types (decodeValue) and does not mirror them. The types
// below (storeConfig, groupConfig, articleConfig, rawStep) are written by hand: a new field of
// fakeserver.Article or Group needs a line there.
type config struct {
	Mode string // "store", "peer", "script" or "raw"

	// Options is fakeserver.Options: Addr, Greeting, NoGreeting, RawStall, TLS (true: implicit
	// TLS with the program's certificate), Script (mode script), StoreOpts (mode store), Faults,
	// Deadline (ms), MaxConns, MaxConnsGreeting. Store and Handler are not settable.
	Options fakeserver.Options

	Store       storeConfig            // mode store: what the store holds
	PeerOptions fakeserver.PeerOptions // mode peer
	Raw         []rawStep              // mode raw: the byte script
}

// storeConfig is the content of the Store of mode store.
type storeConfig struct {
	Groups   []groupConfig
	Articles []articleConfig
}

// groupConfig is one group: Created defaults to 2020-01-01 UTC.
type groupConfig struct {
	Name, Description string
	Created           time.Time // RFC 3339
	NoPosting         bool      // LIST ACTIVE "n"
}

// articleConfig is one fakeserver.Article filed in Group; a zero Date and Arrived default to
// 2020-01-02T03:04:05Z (so that replies are the same on every run).
type articleConfig struct {
	Group      string
	Number     int64
	MessageID  string
	Headers    []string
	Body       []string
	HeadersB64 []string  // instead of Headers: each line in base64, for bytes that are not UTF-8
	BodyB64    []string  // instead of Body, the same
	Date       time.Time // RFC 3339
	Arrived    time.Time
}

// rawStep is one step of mode raw: read one client line (unless Expect is null or absent) that
// matches Expect, then the dot block when ReadDot, write the chunks of Send, wait Sleep, then
// act as Then says.
type rawStep struct {
	Expect  *string       // anchored regexp for one client line; null: read nothing
	ReadDot bool          // after the line, read a dot-terminated block (recorded in articles)
	DotMax  int           // >0 with ReadDot: read only this many lines, then go on
	Send    []rawChunk    // each chunk is its own write
	Sleep   time.Duration // ms, after sending, before Then
	Then    string        // "", close, rst, hang, starttls or inflate
}

// rawChunk is bytes to write: B64 (base64) or Text (UTF-8, JSON escapes give CR, LF and so on).
type rawChunk struct {
	B64      string
	Text     string
	Gap      time.Duration // ms, pause before the chunk
	Split    int           // >0: write in pieces of this many bytes
	SplitGap time.Duration // ms, pause between pieces
}

// thens are the actions after a raw step.
var thens = []string{"", "close", "rst", "hang", "starttls", "inflate"}

var modes = []string{"store", "peer", "script", "raw"}

// parseConfig decodes data and checks that the parts fit the mode. tlsCfg is the server config
// that "TLS": true and an empty StartTLS stand for.
func parseConfig(data []byte, tlsCfg *tls.Config) (*config, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var cfg config
	d := decoder{tls: tlsCfg}
	if err := d.decode(reflect.ValueOf(&cfg).Elem(), raw, "config"); err != nil {
		return nil, err
	}
	if !slices.Contains(modes, cfg.Mode) {
		return nil, fmt.Errorf(`config.Mode: want one of %s, got %q`, strings.Join(modes, ", "), cfg.Mode)
	}
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// check rejects the parts of the config that the mode does not use (a setting that is silently
// ignored is a typo that passes), and bad regexps, base64 and thens in raw mode.
func (cfg *config) check() error {
	o := reflect.ValueOf(&cfg.Options).Elem()
	parts := []struct {
		name string
		v    reflect.Value
		in   string // the modes that use it
	}{
		{"Options.StoreOpts", o.FieldByName("StoreOpts"), "store"},
		{"Store", reflect.ValueOf(&cfg.Store).Elem(), "store"},
		{"PeerOptions", reflect.ValueOf(&cfg.PeerOptions).Elem(), "peer"},
		{"Options.Script", o.FieldByName("Script"), "script"},
		{"Options.Faults", o.FieldByName("Faults"), "store, peer, script"},
		{"Options.Greeting", o.FieldByName("Greeting"), "store, peer, script"},
		{"Raw", reflect.ValueOf(&cfg.Raw).Elem(), "raw"},
	}
	for _, p := range parts {
		if !p.v.IsValid() { // a kit field was renamed or removed: this program needs the change
			return fmt.Errorf("internal error: config.%s no longer exists in the kit", p.name)
		}
		if !isEmpty(p.v) && !strings.Contains(p.in, cfg.Mode) {
			return fmt.Errorf("config.%s is not used in mode %q (only in %s)", p.name, cfg.Mode, p.in)
		}
	}
	if err := checkFaults(cfg.Options.Faults); err != nil {
		return err
	}
	for i, st := range cfg.Options.Script {
		if _, err := regexp.Compile("^(?:" + st.Expect + ")$"); err != nil {
			return fmt.Errorf("config.Options.Script[%d].Expect: %w", i, err)
		}
	}
	if cfg.Mode != "raw" {
		return nil
	}
	for i, st := range cfg.Raw {
		if st.Expect != nil {
			if _, err := regexp.Compile("^(?:" + *st.Expect + ")$"); err != nil {
				return fmt.Errorf("config.Raw[%d].Expect: %w", i, err)
			}
		}
		if !slices.Contains(thens, st.Then) {
			return fmt.Errorf("config.Raw[%d].Then: want one of %q, got %q", i, thens, st.Then)
		}
		for j, ch := range st.Send {
			if _, err := ch.bytes(); err != nil {
				return fmt.Errorf("config.Raw[%d].Send[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}

// isEmpty reports whether v holds nothing a section would use: zero values, nil pointers, and
// maps and slices without elements (an empty section that a caller always writes is fine). A
// non-nil pointer is not empty, whatever it points to: "StartTLS": {} turns STARTTLS on.
func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Map, reflect.Slice:
		return v.Len() == 0
	case reflect.Struct:
		for i := range v.NumField() {
			if !isEmpty(v.Field(i)) {
				return false
			}
		}
		return true
	}
	return v.IsZero()
}

// checkFaults rejects a command key of Faults that is not upper case: the kit looks commands up
// upper-cased, so {"Delay": {"date": 100}} would never fire. OverrideLine is exempt, the kit
// normalises its keys (whole client lines, "article 2" is fine).
func checkFaults(f fakeserver.Faults) error {
	v := reflect.ValueOf(f)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if v.Field(i).Kind() != reflect.Map || name == "OverrideLine" || v.Type().Field(i).Type.Key().Kind() != reflect.String {
			continue
		}
		for _, k := range v.Field(i).MapKeys() {
			if key := k.String(); key != strings.ToUpper(key) {
				return fmt.Errorf("config.Options.Faults.%s[%q]: command names are upper case (%q)", name, key, strings.ToUpper(key))
			}
		}
	}
	return nil
}

// bytes returns the data of the chunk.
func (ch rawChunk) bytes() ([]byte, error) {
	if ch.B64 != "" && ch.Text != "" {
		return nil, fmt.Errorf("B64 and Text are both set")
	}
	if ch.Text != "" {
		return []byte(ch.Text), nil
	}
	b, err := base64.StdEncoding.DecodeString(ch.B64)
	if err != nil {
		return nil, fmt.Errorf("B64: %w", err)
	}
	return b, nil
}

// buildStore fills a Store from the config. AddArticle panics on a setup error (a number taken,
// a Message-ID that differs from the header): that becomes an error.
func buildStore(sc storeConfig) (st *fakeserver.Store, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("config.Store: %v", r)
		}
	}()
	day := func(h, m, s int, d int) time.Time { return time.Date(2020, 1, d, h, m, s, 0, time.UTC) }
	st = fakeserver.NewStore()
	for i, g := range sc.Groups {
		if g.Name == "" {
			return nil, fmt.Errorf("config.Store.Groups[%d].Name is empty", i)
		}
		created := g.Created
		if created.IsZero() {
			created = day(0, 0, 0, 1)
		}
		st.AddGroupAt(g.Name, g.Description, created)
		if g.NoPosting {
			st.SetPosting(g.Name, false)
		}
	}
	for i, a := range sc.Articles {
		if a.Group == "" {
			return nil, fmt.Errorf("config.Store.Articles[%d].Group is empty", i)
		}
		hdr, err := lines(a.Headers, a.HeadersB64)
		if err != nil {
			return nil, fmt.Errorf("config.Store.Articles[%d].Headers: %w", i, err)
		}
		body, err := lines(a.Body, a.BodyB64)
		if err != nil {
			return nil, fmt.Errorf("config.Store.Articles[%d].Body: %w", i, err)
		}
		art := fakeserver.Article{Number: a.Number, MessageID: a.MessageID, Headers: hdr, Body: body, Date: a.Date, Arrived: a.Arrived}
		if art.Date.IsZero() {
			art.Date = day(3, 4, 5, 2)
		}
		if art.Arrived.IsZero() {
			art.Arrived = day(3, 4, 5, 2)
		}
		st.AddArticle(a.Group, art)
	}
	return st, nil
}

// lines returns the text lines, or the lines of the base64 form (not both).
func lines(text, b64 []string) ([]string, error) {
	if len(b64) == 0 {
		return text, nil
	}
	if len(text) > 0 {
		return nil, errors.New("the text form and the B64 form are both set")
	}
	out := make([]string, len(b64))
	for i, l := range b64 {
		b, err := base64.StdEncoding.DecodeString(l)
		if err != nil {
			return nil, fmt.Errorf("B64[%d]: %w", i, err)
		}
		out[i] = string(b)
	}
	return out, nil
}

// decoder fills Go values from JSON by walking their types.
//
// The rules: a struct is a JSON object whose keys are exported field names (exact match; an
// unknown key is an error), a time.Duration is a number of milliseconds (fractions allowed), a
// map has string keys, a slice is an array, scalars are JSON scalars, a type with an
// UnmarshalJSON (time.Time: RFC 3339) uses it, a *tls.Config is true (the program's server
// config) or false. Functions, channels, interfaces and structs without exported fields (Store)
// cannot be set: naming one is an error. JSON null leaves the zero value.
type decoder struct {
	tls *tls.Config
}

var (
	durationType    = reflect.TypeFor[time.Duration]()
	tlsConfigType   = reflect.TypeFor[tls.Config]()
	startTLSType    = reflect.TypeFor[fakeserver.StartTLSOptions]()
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
)

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// pathError is an error at a place in the document.
type pathError struct {
	path string
	err  error
}

func (e *pathError) Error() string { return e.path + ": " + e.err.Error() }
func (e *pathError) Unwrap() error { return e.err }

// decode fills dst from raw; an error names the innermost path.
func (d *decoder) decode(dst reflect.Value, raw json.RawMessage, path string) error {
	err := d.decodeValue(dst, raw, path)
	var pe *pathError
	if err != nil && !errors.As(err, &pe) {
		return &pathError{path, err}
	}
	return err
}

func (d *decoder) decodeValue(dst reflect.Value, raw json.RawMessage, path string) error {
	t := dst.Type()
	switch {
	case t == durationType:
		var ms float64
		if err := json.Unmarshal(raw, &ms); err != nil {
			return fmt.Errorf("want a number of milliseconds")
		}
		if math.Abs(ms) > float64(math.MaxInt64/int64(time.Millisecond)) {
			return fmt.Errorf("%v ms is out of range", ms)
		}
		dst.SetInt(int64(math.Round(ms * float64(time.Millisecond))))
		return nil
	case t.Kind() == reflect.Pointer && t.Elem() == tlsConfigType:
		var on bool
		if isNull(raw) {
			return nil
		}
		if err := json.Unmarshal(raw, &on); err != nil {
			return fmt.Errorf("want true (the server certificate of this program) or false")
		}
		if on {
			dst.Set(reflect.ValueOf(d.tls))
		}
		return nil
	case isNull(raw):
		return nil
	case t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(unmarshalerType):
		return json.Unmarshal(raw, dst.Addr().Interface())
	}
	switch t.Kind() {
	case reflect.Pointer:
		v := reflect.New(t.Elem())
		if err := d.decodeValue(v.Elem(), raw, path); err != nil {
			return err
		}
		dst.Set(v)
	case reflect.Struct:
		return d.decodeStruct(dst, raw, path)
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return fmt.Errorf("a map with %s keys cannot be set from JSON", t.Key())
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("want an object")
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for _, k := range sortedKeys(m) {
			e := reflect.New(t.Elem()).Elem()
			if err := d.decode(e, m[k], fmt.Sprintf("%s[%q]", path, k)); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), e)
		}
		dst.Set(out)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 { // []byte is a base64 string, as in encoding/json
			if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) || json.Unmarshal(raw, dst.Addr().Interface()) != nil {
				return fmt.Errorf("want a base64 string")
			}
			return nil
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return fmt.Errorf("want an array")
		}
		out := reflect.MakeSlice(t, len(arr), len(arr))
		for i, r := range arr {
			if err := d.decode(out.Index(i), r, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		dst.Set(out)
	case reflect.Bool, reflect.String, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if err := json.Unmarshal(raw, dst.Addr().Interface()); err != nil {
			return fmt.Errorf("want %s", t.Kind())
		}
	default:
		return fmt.Errorf("cannot be set from JSON (type %s)", t)
	}
	return nil
}

func (d *decoder) decodeStruct(dst reflect.Value, raw json.RawMessage, path string) error {
	t := dst.Type()
	var known []string
	for i := range t.NumField() {
		if t.Field(i).IsExported() {
			known = append(known, t.Field(i).Name)
		}
	}
	if len(known) == 0 {
		return fmt.Errorf("cannot be set from JSON (type %s)", t)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("want an object")
	}
	if t == startTLSType {
		// STARTTLS needs a config: this program's, unless "Config": false switches it off.
		if _, set := m["Config"]; !set {
			dst.FieldByName("Config").Set(reflect.ValueOf(d.tls))
		}
	}
	for _, k := range sortedKeys(m) {
		f, ok := t.FieldByName(k)
		if !ok || !f.IsExported() || len(f.Index) != 1 {
			return &pathError{path + "." + k, fmt.Errorf("unknown key (the keys of %s are %s)", t.Name(), strings.Join(known, ", "))}
		}
		if err := d.decode(dst.FieldByIndex(f.Index), m[k], path+"."+k); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
