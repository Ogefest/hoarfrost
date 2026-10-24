package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/blocklist"
	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/index"
)

// settingsForm is config.Settings as form fields. Empty values inherit;
// Fallback "none" disables the fallback engine.
type settingsForm struct {
	Engine       string
	Fallback     string
	Poll         string
	MaxAge       string
	Storage      string
	Priority     string
	MinTextChars string
	MaxSize      string
	Attachments  string
	Block        string
	SkipFrames   string
	UserAgent    string
	ChromeScroll string
	ChromeWait   string
}

func toSettingsForm(st config.Settings) settingsForm {
	f := settingsForm{Engine: st.Engine, Storage: st.Storage, UserAgent: st.UserAgent,
		Block: strings.Join(st.Block, "\n"), SkipFrames: strings.Join(st.SkipFrames, "\n")}
	if st.Fallback != nil {
		f.Fallback = *st.Fallback
		if f.Fallback == "" {
			f.Fallback = "none"
		}
	}
	if st.Poll > 0 {
		f.Poll = humanDuration(time.Duration(st.Poll))
	}
	if st.MaxAge > 0 {
		f.MaxAge = humanDuration(time.Duration(st.MaxAge))
	}
	if st.Priority != nil {
		f.Priority = strconv.Itoa(*st.Priority)
	}
	if st.MinTextChars != nil {
		f.MinTextChars = strconv.Itoa(*st.MinTextChars)
	}
	if st.MaxSize > 0 {
		f.MaxSize = formSize(int64(st.MaxSize))
	}
	f.Attachments = yesNo(st.Attachments)
	if c := st.Chrome; c != nil {
		f.ChromeScroll = yesNo(c.Scroll)
		if c.Wait != nil {
			f.ChromeWait = humanDuration(time.Duration(*c.Wait))
		}
	}
	return f
}

func readSettingsForm(r *http.Request) settingsForm {
	v := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	return settingsForm{Engine: v("engine"), Fallback: v("fallback"), Poll: v("poll"), MaxAge: v("max_age"), Storage: v("storage"),
		Priority: v("priority"), MinTextChars: v("min_text_chars"), MaxSize: v("max_size"), Attachments: v("attachments"),
		Block: v("block"), SkipFrames: v("skip_frames"), UserAgent: v("user_agent"), ChromeScroll: v("chrome_scroll"), ChromeWait: v("chrome_wait")}
}

// settings converts the form back, reporting the first invalid field.
func (f settingsForm) settings() (config.Settings, error) {
	st := config.Settings{Engine: f.Engine, Storage: f.Storage, UserAgent: f.UserAgent, Block: strings.Fields(f.Block), SkipFrames: strings.Fields(f.SkipFrames)}
	switch f.Fallback {
	case "":
	case "none":
		st.Fallback = new(string)
	default:
		st.Fallback = &f.Fallback
	}
	var err error
	if st.Poll, err = formDuration("poll interval", f.Poll); err != nil {
		return st, err
	}
	if st.MaxAge, err = formDuration("maximum age", f.MaxAge); err != nil {
		return st, err
	}
	if st.Priority, err = formInt("priority", f.Priority); err != nil {
		return st, err
	}
	if st.MinTextChars, err = formInt("minimum text length", f.MinTextChars); err != nil {
		return st, err
	}
	if f.MaxSize != "" {
		n, err := config.ParseByteSize(f.MaxSize)
		if err != nil || n <= 0 {
			return st, fmt.Errorf("maximum response size: invalid size %q", f.MaxSize)
		}
		st.MaxSize = config.ByteSize(n)
	}
	st.Attachments = formBool(f.Attachments)
	if scroll, wait := formBool(f.ChromeScroll), f.ChromeWait; scroll != nil || wait != "" {
		st.Chrome = &config.Chrome{Scroll: scroll}
		if wait != "" {
			d, err := formDuration("chrome wait", wait)
			if err != nil {
				return st, err
			}
			st.Chrome.Wait = &d
		}
	}
	return st, nil
}

func formDuration(field, s string) (config.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := config.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q (examples: 30m, 2h, 7d)", field, s)
	}
	return config.Duration(d), nil
}

func formInt(field, s string) (*int, error) {
	if s == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%s: invalid number %q", field, s)
	}
	return &n, nil
}

func formBool(s string) *bool {
	switch s {
	case "yes":
		return new(true)
	case "no":
		return new(false)
	}
	return nil
}

func yesNo(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "yes"
	}
	return "no"
}

func formSize(n int64) string {
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n%u.size == 0 {
			return fmt.Sprintf("%d%s", n/u.size, u.suffix)
		}
	}
	return strconv.FormatInt(n, 10)
}

type sourceForm struct {
	Name     string
	Enabled  bool
	Feeds    string
	Sitemaps string
	URLs     string
	Settings settingsForm
}

func toSourceForm(src config.Source) sourceForm {
	return sourceForm{Name: src.Name, Enabled: src.IsEnabled(), Feeds: strings.Join(src.Feeds, "\n"),
		Sitemaps: strings.Join(src.Sitemaps, "\n"), URLs: strings.Join(src.URLs, "\n"), Settings: toSettingsForm(src.Settings)}
}

func readSourceForm(r *http.Request) sourceForm {
	return sourceForm{Name: strings.TrimSpace(r.PostFormValue("name")), Enabled: r.PostFormValue("enabled") != "",
		Feeds: r.PostFormValue("feeds"), Sitemaps: r.PostFormValue("sitemaps"), URLs: r.PostFormValue("urls"), Settings: readSettingsForm(r)}
}

func (f sourceForm) source() (config.Source, error) {
	st, err := f.Settings.settings()
	src := config.Source{Name: f.Name, Feeds: strings.Fields(f.Feeds), Sitemaps: strings.Fields(f.Sitemaps), URLs: strings.Fields(f.URLs), Settings: st}
	if !f.Enabled {
		src.Enabled = new(false)
	}
	return src, err
}

// formChoices are the select options shared by the forms.
type formChoices struct {
	Engines  []string
	Storages []string
	// Inherited shows what an empty field means.
	Inherited config.Resolved
}

func (s *Server) choices() formChoices {
	return formChoices{Engines: []string{engine.Static, engine.Chrome, engine.HTTP}, Storages: s.Coord.Storages(), Inherited: s.Coord.Sources().Resolve("")}
}

type sourceRow struct {
	Source   config.Source
	Resolved config.Resolved
	Status   coordinator.SourceStatus
	NextPoll time.Time
	Stats    index.SourceStats
}

type sourcesData struct {
	Rows       []sourceRow
	Defaults   config.Resolved
	Blocklists []blocklist.Status
	Rules      int
	Version    string
}

func (s *Server) sourceRows(ctx context.Context) []sourceRow {
	src := s.Coord.Sources()
	stats, _ := s.idx.SourceStats(ctx)
	byName := map[string]index.SourceStats{}
	for _, ss := range stats {
		byName[ss.Source] = ss
	}
	statuses := s.Coord.SourceStatuses(ctx)
	var rows []sourceRow
	for _, so := range src.Sources {
		row := sourceRow{Source: so, Resolved: src.Resolve(so.Name), Status: statuses[so.Name], Stats: byName[so.Name]}
		if so.IsEnabled() {
			row.NextPoll = time.Now()
			if !row.Status.LastPoll.IsZero() {
				row.NextPoll = row.Status.LastPoll.Add(row.Resolved.Poll)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) sourcesPage(w http.ResponseWriter, r *http.Request) {
	d := sourcesData{Rows: s.sourceRows(r.Context()), Defaults: s.Coord.Sources().Resolve(""), Blocklists: s.Coord.BlocklistStatuses()}
	if set, err := s.Coord.Blocklist(r.Context()); err == nil {
		d.Rules, d.Version = set.Len(), set.Version()
	}
	s.render(w, r, "sources.html", "Sources", d)
}

type sourceFormData struct {
	New     bool
	Form    sourceForm
	Choices formChoices
}

func (s *Server) newSourcePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "source.html", "New source", sourceFormData{New: true, Form: sourceForm{Enabled: true}, Choices: s.choices()})
}

func (s *Server) editSourcePage(w http.ResponseWriter, r *http.Request) {
	src, ok := s.Coord.Sources().Find(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "source.html", "Source "+src.Name, sourceFormData{Form: toSourceForm(src), Choices: s.choices()})
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	s.saveSourceForm(w, r, true)
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Coord.Sources().Find(r.PathValue("name")); !ok {
		http.NotFound(w, r)
		return
	}
	s.saveSourceForm(w, r, false)
}

func (s *Server) saveSourceForm(w http.ResponseWriter, r *http.Request, create bool) {
	f := readSourceForm(r)
	if !create {
		f.Name = r.PathValue("name")
	}
	src, err := f.source()
	if err == nil && create {
		if _, exists := s.Coord.Sources().Find(src.Name); exists {
			err = fmt.Errorf("source %q already exists", src.Name)
		}
	}
	if err == nil {
		err = s.Coord.SaveSource(r.Context(), src)
	}
	if err != nil {
		r.URL.RawQuery = "err=" + urlQueryEscape(err.Error())
		s.renderStatus(w, r, http.StatusBadRequest, "source.html", "Source", sourceFormData{New: create, Form: f, Choices: s.choices()})
		return
	}
	redirect(w, r, "/sources", fmt.Sprintf("Source %s saved.", src.Name))
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Coord.DeleteSource(r.Context(), name); err != nil {
		redirectErr(w, r, "/sources", err)
		return
	}
	redirect(w, r, "/sources", fmt.Sprintf("Source %s deleted; its archived pages stay.", name))
}

func (s *Server) toggleSource(w http.ResponseWriter, r *http.Request) {
	src, ok := s.Coord.Sources().Find(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.Coord.SetSourceEnabled(r.Context(), src.Name, !src.IsEnabled()); err != nil {
		redirectErr(w, r, "/sources", err)
		return
	}
	redirect(w, r, "/sources", "Source updated.")
}

// testSource checks a saved source, or the source form posted to
// /sources/test, without queueing anything.
func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	var src config.Source
	if name := r.PathValue("name"); name != "" {
		var ok bool
		if src, ok = s.Coord.Sources().Find(name); !ok {
			http.NotFound(w, r)
			return
		}
	} else {
		var err error
		if src, err = readSourceForm(r).source(); err != nil {
			s.plain(w, err.Error())
			return
		}
		if src.Name == "" {
			src.Name = "test"
		}
		trial := config.Sources{Sources: []config.Source{src}}
		if err := trial.Validate(); err != nil {
			s.plain(w, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	found, fresh, err := s.Coord.TestSource(ctx, src)
	msg := fmt.Sprintf("%d URLs found, %d new", found, fresh)
	if err != nil {
		msg += " — errors: " + err.Error()
	}
	s.plain(w, msg)
}

func (s *Server) plain(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(msg))
}

func (s *Server) pollSource(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	if err := s.Coord.PollSource(ctx, r.PathValue("name")); err != nil {
		redirectErr(w, r, "/sources", err)
		return
	}
	redirect(w, r, "/sources", "Source polled.")
}

func (s *Server) pollSources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	if err := s.Coord.PollSources(ctx, true); err != nil {
		redirectErr(w, r, "/sources", err)
		return
	}
	redirect(w, r, "/sources", "Sources polled.")
}

func (s *Server) updateBlocklists(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	if err := s.Coord.UpdateBlocklists(ctx); err != nil {
		redirectErr(w, r, "/sources/settings", err)
		return
	}
	redirect(w, r, "/sources/settings", "Block lists updated.")
}

type settingsData struct {
	Defaults     settingsForm
	MaxPerDomain string
	ListURLs     string
	Refresh      string
	Allow        string
	DefaultLists bool
	Blocklists   []blocklist.Status
	Rules        int
	Version      string
	Choices      formChoices
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, nil)
}

// renderSettings shows the stored settings, or the posted forms in d when a
// save failed.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, d *settingsData) {
	cfg := s.Coord.Sources()
	if d == nil {
		urls, refresh, allow := cfg.BlocklistSettings()
		d = &settingsData{Defaults: toSettingsForm(cfg.Defaults.Settings), ListURLs: strings.Join(urls, "\n"),
			Refresh: humanDuration(refresh), Allow: strings.Join(allow, "\n"), DefaultLists: cfg.Blocklists == nil}
		if cfg.Defaults.MaxPerDomain > 0 {
			d.MaxPerDomain = strconv.Itoa(cfg.Defaults.MaxPerDomain)
		}
	}
	d.Choices = s.choices()
	// Empty default fields fall back to the built-in values.
	d.Choices.Inherited = (&config.Sources{}).Resolve("")
	d.Blocklists = s.Coord.BlocklistStatuses()
	if set, err := s.Coord.Blocklist(r.Context()); err == nil {
		d.Rules, d.Version = set.Len(), set.Version()
	}
	s.renderStatus(w, r, status, "settings.html", "Capture settings", d)
}

func (s *Server) saveDefaults(w http.ResponseWriter, r *http.Request) {
	f := readSettingsForm(r)
	perDomain := strings.TrimSpace(r.PostFormValue("max_per_domain"))
	st, err := f.settings()
	d := config.Defaults{Settings: st}
	if err == nil && perDomain != "" {
		var n *int
		if n, err = formInt("parallel captures per domain", perDomain); err == nil {
			d.MaxPerDomain = *n
		}
	}
	if err == nil {
		err = s.Coord.SetDefaults(r.Context(), d)
	}
	if err != nil {
		r.URL.RawQuery = "err=" + urlQueryEscape(err.Error())
		cur := s.Coord.Sources()
		urls, refresh, allow := cur.BlocklistSettings()
		s.renderSettings(w, r, http.StatusBadRequest, &settingsData{Defaults: f, MaxPerDomain: perDomain, ListURLs: strings.Join(urls, "\n"),
			Refresh: humanDuration(refresh), Allow: strings.Join(allow, "\n"), DefaultLists: cur.Blocklists == nil})
		return
	}
	redirect(w, r, "/sources/settings", "Defaults saved.")
}

func (s *Server) saveBlocklistSettings(w http.ResponseWriter, r *http.Request) {
	urls, refresh, allow := r.PostFormValue("urls"), strings.TrimSpace(r.PostFormValue("refresh")), r.PostFormValue("allow")
	b := &config.Blocklists{URLs: strings.Fields(urls), Allow: strings.Fields(allow)}
	d, err := formDuration("refresh interval", refresh)
	b.Refresh = d
	if err == nil {
		if r.PostFormValue("reset") != "" {
			b = nil
		}
		err = s.Coord.SetBlocklists(r.Context(), b)
	}
	if err != nil {
		r.URL.RawQuery = "err=" + urlQueryEscape(err.Error())
		cur := s.Coord.Sources()
		data := &settingsData{Defaults: toSettingsForm(cur.Defaults.Settings), ListURLs: urls, Refresh: refresh, Allow: allow}
		if cur.Defaults.MaxPerDomain > 0 {
			data.MaxPerDomain = strconv.Itoa(cur.Defaults.MaxPerDomain)
		}
		s.renderSettings(w, r, http.StatusBadRequest, data)
		return
	}
	redirect(w, r, "/sources/settings", "Block list settings saved; the lists are downloaded within a minute.")
}

// apiSource is a source with its poll state.
type apiSource struct {
	config.Source
	LastPoll  *time.Time `json:"lastPoll,omitempty"`
	NextPoll  *time.Time `json:"nextPoll,omitempty"`
	Found     int        `json:"found"`
	New       int        `json:"new"`
	LastError string     `json:"lastError,omitempty"`
}

func (s *Server) apiSources(w http.ResponseWriter, r *http.Request) {
	cfg := s.Coord.Sources()
	out := []apiSource{}
	for _, row := range s.sourceRows(r.Context()) {
		a := apiSource{Source: row.Source, Found: row.Status.Found, New: row.Status.New, LastError: row.Status.Error}
		if !row.Status.LastPoll.IsZero() {
			a.LastPoll = &row.Status.LastPoll
		}
		if !row.NextPoll.IsZero() {
			a.NextPoll = &row.NextPoll
		}
		out = append(out, a)
	}
	urls, refresh, allow := cfg.BlocklistSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"sources":    out,
		"defaults":   cfg.Defaults,
		"blocklists": config.Blocklists{URLs: urls, Refresh: config.Duration(refresh), Allow: allow},
	})
}

// decodeStrict reads a JSON body, rejecting unknown fields.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) apiPutSource(w http.ResponseWriter, r *http.Request) {
	var src config.Source
	if err := decodeStrict(r, &src); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	if src.Name != "" && src.Name != r.PathValue("name") {
		jsonError(w, http.StatusBadRequest, errors.New("name in the body differs from the URL"))
		return
	}
	src.Name = r.PathValue("name")
	_, exists := s.Coord.Sources().Find(src.Name)
	if err := s.Coord.SaveSource(r.Context(), src); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	writeJSON(w, status, src)
}

func (s *Server) apiDeleteSource(w http.ResponseWriter, r *http.Request) {
	if err := s.Coord.DeleteSource(r.Context(), r.PathValue("name")); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, coordinator.ErrUnknownSource) {
			code = http.StatusNotFound
		}
		jsonError(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiPollSource(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	err := s.Coord.PollSource(ctx, r.PathValue("name"))
	if errors.Is(err, coordinator.ErrUnknownSource) {
		jsonError(w, http.StatusNotFound, err)
		return
	}
	st := s.Coord.SourceStatuses(ctx)[r.PathValue("name")]
	writeJSON(w, http.StatusOK, map[string]any{"found": st.Found, "new": st.New, "error": st.Error})
}

func (s *Server) apiPutDefaults(w http.ResponseWriter, r *http.Request) {
	var d config.Defaults
	if err := decodeStrict(r, &d); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Coord.SetDefaults(r.Context(), d); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// apiPutBlocklists replaces the block list settings; a JSON null restores
// the default list.
func (s *Server) apiPutBlocklists(w http.ResponseWriter, r *http.Request) {
	var b *config.Blocklists
	if err := decodeStrict(r, &b); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	if b != nil && b.URLs == nil {
		b.URLs = []string{}
	}
	if err := s.Coord.SetBlocklists(r.Context(), b); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) apiUpdateBlocklists(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	if err := s.Coord.UpdateBlocklists(ctx); err != nil {
		jsonError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Coord.BlocklistStatuses())
}
