package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ogefest/hoarfrost/internal/engine"
)

// Duration is a YAML duration such as "1h" or "3s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30m\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a YAML size such as "50MB".
type ByteSize int64

var sizeRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([KMGT]?I?B?)?$`)

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

func (b ByteSize) MarshalYAML() (any, error) { return int64(b), nil }

// UnmarshalJSON accepts a number of bytes or a string such as "50MB".
func (b *ByteSize) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("invalid size %s", raw)
		}
		*b = ByteSize(n)
		return nil
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

// ParseByteSize parses sizes like 1024, 10KB, 50MB, 2GiB.
func ParseByteSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	mult := map[string]float64{"": 1, "B": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}
	unit := strings.TrimSuffix(strings.TrimSuffix(m[2], "B"), "I")
	return int64(f * mult[unit]), nil
}

// Chrome configures the chrome engine.
type Chrome struct {
	Scroll *bool     `yaml:"scroll,omitempty" json:"scroll,omitempty"`
	Wait   *Duration `yaml:"wait,omitempty" json:"wait,omitempty"`
}

// Settings are capture settings that sources inherit from defaults.
type Settings struct {
	Engine       string   `yaml:"engine,omitempty" json:"engine,omitempty"`
	Fallback     *string  `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	Poll         Duration `yaml:"poll,omitempty" json:"poll,omitempty"`
	MinTextChars *int     `yaml:"min_text_chars,omitempty" json:"min_text_chars,omitempty"`
	Block        []string `yaml:"block,omitempty" json:"block,omitempty"`
	SkipFrames   []string `yaml:"skip_frames,omitempty" json:"skip_frames,omitempty"`
	MaxSize      ByteSize `yaml:"max_size,omitempty" json:"max_size,omitempty"`
	Attachments  *bool    `yaml:"attachments,omitempty" json:"attachments,omitempty"`
	Priority     *int     `yaml:"priority,omitempty" json:"priority,omitempty"`
	UserAgent    string   `yaml:"user_agent,omitempty" json:"user_agent,omitempty"`
	// Storage names the storage (HOARFROST_STORAGES) captures are written to.
	Storage string  `yaml:"storage,omitempty" json:"storage,omitempty"`
	Chrome  *Chrome `yaml:"chrome,omitempty" json:"chrome,omitempty"`
	// MaxAge skips feed and sitemap entries older than this (when dated).
	MaxAge Duration `yaml:"max_age,omitempty" json:"max_age,omitempty"`
}

// Defaults apply to every source.
type Defaults struct {
	Settings     `yaml:",inline"`
	MaxPerDomain int `yaml:"max_per_domain,omitempty" json:"max_per_domain,omitempty"`
}

// Source is a named set of feeds, sitemaps and URLs.
type Source struct {
	Name     string   `yaml:"name" json:"name"`
	Enabled  *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Feeds    []string `yaml:"feeds,omitempty" json:"feeds,omitempty"`
	Sitemaps []string `yaml:"sitemaps,omitempty" json:"sitemaps,omitempty"`
	URLs     []string `yaml:"urls,omitempty" json:"urls,omitempty"`
	Settings `yaml:",inline"`
}

// IsEnabled reports whether the source is polled.
func (s Source) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Blocklists configures public block lists (Pi-hole hosts files, domain
// lists, adblock rules) whose domains are never captured.
type Blocklists struct {
	URLs    []string `yaml:"urls" json:"urls"`
	Refresh Duration `yaml:"refresh,omitempty" json:"refresh,omitempty"`
	// Allow lists domains that are never blocked by the lists.
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
}

// Sources is the capture configuration. It is kept in app.sqlite and edited
// in the web UI or through the API; every version is copied to storage as
// YAML (config/sources/) so each archive documents how it was collected.
type Sources struct {
	Defaults Defaults `yaml:"defaults" json:"defaults"`
	// Blocklists defaults to the Pi-hole default list when omitted; an
	// empty urls list disables block lists.
	Blocklists *Blocklists `yaml:"blocklists,omitempty" json:"blocklists,omitempty"`
	Sources    []Source    `yaml:"sources" json:"sources"`
}

// DefaultBlocklistURLs is used when block lists were never configured.
var DefaultBlocklistURLs = []string{"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"}

// BlocklistSettings returns the effective block list configuration.
func (s *Sources) BlocklistSettings() (urls []string, refresh time.Duration, allow []string) {
	refresh = 24 * time.Hour
	if s.Blocklists == nil {
		return DefaultBlocklistURLs, refresh, nil
	}
	if s.Blocklists.Refresh > 0 {
		refresh = time.Duration(s.Blocklists.Refresh)
	}
	return s.Blocklists.URLs, refresh, s.Blocklists.Allow
}

// Built-in defaults used when the configuration leaves values unset.
const (
	DefaultPoll         = time.Hour
	DefaultMaxPerDomain = 2
	DefaultMinTextChars = 500
	DefaultMaxSize      = 100 << 20
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var storageNameRe = nameRe

// ValidStorageName reports whether s can name a storage.
func ValidStorageName(s string) bool { return storageNameRe.MatchString(s) }

// StorageNames lists storages referenced by the configuration.
func (s *Sources) StorageNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(s.Defaults.Storage)
	for _, src := range s.Sources {
		add(src.Storage)
	}
	return out
}

// ParseSources parses and validates a YAML configuration, such as a copy
// stored in config/sources/.
func ParseSources(b []byte) (*Sources, error) {
	var s Sources
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("sources: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Marshal renders the configuration as YAML.
func (s *Sources) Marshal() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// SnapshotKey is the storage key for a configuration copy.
func SnapshotKey(t time.Time) string {
	return "config/sources/" + t.UTC().Format("2006-01-02T150405Z") + ".yaml"
}

var knownEngines = map[string]bool{engine.HTTP: true, engine.Static: true, engine.Chrome: true}

// Validate checks names, engines and URLs.
func (s *Sources) Validate() error {
	var errs []error
	checkSettings := func(where string, st Settings) {
		if st.Storage != "" && !storageNameRe.MatchString(st.Storage) {
			errs = append(errs, fmt.Errorf("%s: invalid storage name %q", where, st.Storage))
		}
		if st.Engine != "" && !knownEngines[st.Engine] {
			errs = append(errs, fmt.Errorf("%s: unknown engine %q", where, st.Engine))
		}
		if st.Fallback != nil && *st.Fallback != "" && !knownEngines[*st.Fallback] {
			errs = append(errs, fmt.Errorf("%s: unknown fallback engine %q", where, *st.Fallback))
		}
		if st.Poll < 0 || (st.Poll > 0 && time.Duration(st.Poll) < time.Minute) {
			errs = append(errs, fmt.Errorf("%s: poll interval must be at least 1m", where))
		}
	}
	checkSettings("defaults", s.Defaults.Settings)
	if b := s.Blocklists; b != nil {
		for _, u := range b.URLs {
			if err := ValidateURL(u); err != nil {
				errs = append(errs, fmt.Errorf("blocklists: %w", err))
			}
		}
		if b.Refresh > 0 && time.Duration(b.Refresh) < 10*time.Minute {
			errs = append(errs, errors.New("blocklists: refresh must be at least 10m"))
		}
	}
	if s.Defaults.MaxPerDomain < 0 {
		errs = append(errs, errors.New("defaults: max_per_domain must not be negative"))
	}
	seen := map[string]bool{}
	for i, src := range s.Sources {
		where := fmt.Sprintf("sources[%d]", i)
		if !nameRe.MatchString(src.Name) {
			errs = append(errs, fmt.Errorf("%s: name %q must match %s", where, src.Name, nameRe))
		} else {
			where = "source " + src.Name
		}
		if seen[src.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", where))
		}
		seen[src.Name] = true
		if len(src.Feeds)+len(src.Sitemaps)+len(src.URLs) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs feeds, sitemaps or urls", where))
		}
		for _, list := range [][]string{src.Feeds, src.Sitemaps, src.URLs} {
			for _, u := range list {
				if err := ValidateURL(u); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", where, err))
				}
			}
		}
		checkSettings(where, src.Settings)
	}
	return errors.Join(errs...)
}

// ValidateURL accepts absolute http(s) URLs.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	return nil
}

// Clone returns a copy whose source list can be modified independently.
func (s *Sources) Clone() *Sources {
	c := *s
	c.Sources = append([]Source(nil), s.Sources...)
	return &c
}

// Find returns the named source.
func (s *Sources) Find(name string) (Source, bool) {
	for _, src := range s.Sources {
		if src.Name == name {
			return src, true
		}
	}
	return Source{}, false
}

// MaxPerDomain returns the per-domain lease limit.
func (s *Sources) MaxPerDomain() int {
	if s.Defaults.MaxPerDomain > 0 {
		return s.Defaults.MaxPerDomain
	}
	return DefaultMaxPerDomain
}

// Resolved are the effective settings of a source.
type Resolved struct {
	Engine   string
	Storage  string
	Poll     time.Duration
	Priority int
	MaxAge   time.Duration
	Options  engine.Options
}

// Resolve merges source settings over defaults. Unknown names (e.g. URLs
// added through the API) get the defaults.
func (s *Sources) Resolve(name string) Resolved {
	d := s.Defaults.Settings
	src, _ := s.Find(name)
	st := src.Settings
	r := Resolved{Engine: first(st.Engine, d.Engine, engine.Static), Storage: first(st.Storage, d.Storage, "default"), Poll: DefaultPoll}
	if d.Poll > 0 {
		r.Poll = time.Duration(d.Poll)
	}
	if st.Poll > 0 {
		r.Poll = time.Duration(st.Poll)
	}
	r.Priority = derefOr(st.Priority, derefOr(d.Priority, 100))
	r.MaxAge = time.Duration(d.MaxAge)
	if st.MaxAge > 0 {
		r.MaxAge = time.Duration(st.MaxAge)
	}
	o := &r.Options
	o.Fallback = derefOr(st.Fallback, derefOr(d.Fallback, ""))
	o.MinTextChars = derefOr(st.MinTextChars, derefOr(d.MinTextChars, DefaultMinTextChars))
	o.Block = append(append([]string{}, d.Block...), st.Block...)
	o.SkipFrames = append(append([]string{}, d.SkipFrames...), st.SkipFrames...)
	o.MaxResponseSize = int64(DefaultMaxSize)
	if d.MaxSize > 0 {
		o.MaxResponseSize = int64(d.MaxSize)
	}
	if st.MaxSize > 0 {
		o.MaxResponseSize = int64(st.MaxSize)
	}
	o.Attachments = derefOr(st.Attachments, derefOr(d.Attachments, true))
	o.UserAgent = first(st.UserAgent, d.UserAgent)
	for _, c := range []*Chrome{d.Chrome, st.Chrome} {
		if c == nil {
			continue
		}
		if c.Scroll != nil {
			o.ChromeScroll = *c.Scroll
		}
		if c.Wait != nil {
			o.ChromeWait = time.Duration(*c.Wait)
		}
	}
	if o.Fallback == r.Engine {
		o.Fallback = ""
	}
	return r
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func derefOr[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
