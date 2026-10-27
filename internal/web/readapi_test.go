package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ogefest/hoarfrost/docs"
	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/epoch"
)

var apiKeyRe = regexp.MustCompile(`hfr_[A-Za-z0-9_-]+`)

func TestReadOnlyAPIKeys(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.admin(t)
	editor, _ := e.auth.CreateUser(ctx, "editor", "editor-password", auth.RoleEditor)
	etok, _ := e.auth.CreateToken(ctx, editor, "ci")
	for _, p := range []string{"/news/article.html", "/news/article2.html"} {
		if resp, body := api(t, "POST", e.srv.URL+"/api/urls", etok, "application/json", strings.NewReader(`{"url":"`+e.site.URL+p+`","source":"wiadomosci"}`)); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("add url: %d %s", resp.StatusCode, body)
		}
	}
	e.drain()

	b.get(t, "/system/keys")
	resp, body := b.post(t, "/system/keys", url.Values{"csrf": {b.csrf}, "name": {"dashboard"}})
	key := apiKeyRe.FindString(body)
	if resp.StatusCode != 200 || key == "" || !strings.Contains(body, "dashboard") {
		t.Fatalf("create key: %d", resp.StatusCode)
	}
	if resp, _ := b.post(t, "/system/keys", url.Values{"csrf": {b.csrf}, "name": {"old"}, "expires": {"2001-01-01"}}); !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatal("key expiring in the past")
	}

	// Incremental listing with a cursor.
	var list struct {
		Results []apiPage `json:"results"`
		Next    int64     `json:"next"`
	}
	resp, raw := api(t, "GET", e.srv.URL+"/api/pages?after=0&limit=1", key, "", nil)
	json.Unmarshal(raw, &list)
	if resp.StatusCode != 200 || len(list.Results) != 1 || list.Next != list.Results[0].ID {
		t.Fatalf("pages: %d %s", resp.StatusCode, raw)
	}
	first := list.Results[0]
	seen := map[string]apiPage{first.CaptureID: first}
	for range 10 {
		_, raw = api(t, "GET", fmt.Sprintf("%s/api/pages?after=%d&source=wiadomosci", e.srv.URL, list.Next), key, "", nil)
		prev := list.Next
		list.Results = nil
		json.Unmarshal(raw, &list)
		if len(list.Results) == 0 {
			if list.Next != prev {
				t.Fatalf("cursor moved on an empty page: %s", raw)
			}
			break
		}
		for _, p := range list.Results {
			if p.ID <= prev {
				t.Fatalf("listing went back: %s", raw)
			}
			seen[p.CaptureID] = p
		}
	}
	var pdf apiPage
	for _, p := range seen {
		if p.Kind == "attachment" {
			pdf = p
		}
	}
	if len(seen) != 3 || pdf.Links.Content == "" || pdf.Links.HTML != "" {
		t.Fatalf("listing: %+v", seen)
	}
	if resp, raw := api(t, "GET", e.srv.URL+pdf.Links.Content, key, "", nil); resp.StatusCode != 200 ||
		resp.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(string(raw), "%PDF") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "uchwala.pdf") {
		t.Fatalf("pdf content: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/api/pages?after=x", key, "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("bad cursor")
	}

	// A page by stable capture ID, its captured file and HTML export.
	if first.CaptureID == "" || strings.ContainsAny(first.CaptureID, "<>:") || first.Links.Content == "" {
		t.Fatalf("capture id and links: %+v", first)
	}
	resp, raw = api(t, "GET", e.srv.URL+first.Links.Self, key, "", nil)
	var pg apiPage
	json.Unmarshal(raw, &pg)
	if resp.StatusCode != 200 || pg.ID != first.ID || pg.Text == "" {
		t.Fatalf("page by capture id: %d %s", resp.StatusCode, raw)
	}
	resp, raw = api(t, "GET", e.srv.URL+first.Links.Content, key, "", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(string(raw), "<title>") ||
		resp.Header.Get("X-Hoarfrost-URL") != first.URL {
		t.Fatalf("content: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := api(t, "GET", e.srv.URL+first.Links.HTML, key, "", nil); resp.StatusCode != 200 {
		t.Fatalf("export by capture id: %d", resp.StatusCode)
	}
	if resp, raw := api(t, "GET", e.srv.URL+"/metrics", key, "", nil); resp.StatusCode != 200 || !strings.Contains(string(raw), "hoarfrost_seal_errors") {
		t.Fatalf("metrics with an API key: %d", resp.StatusCode)
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/api/pages/00000000-0000-0000-0000-000000000000", key, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("unknown capture")
	}
	if resp, raw := api(t, "GET", e.srv.URL+"/api/search?q=tramwajowej", key, "", nil); resp.StatusCode != 200 || !strings.Contains(string(raw), `"total":1`) {
		t.Fatalf("search: %s", raw)
	}
	if resp, raw := api(t, "GET", e.srv.URL+"/api/pages?after=0&keyword=INWESTYCJE", key, "", nil); resp.StatusCode != 200 ||
		!strings.Contains(string(raw), `"keywords":["Transport","Inwestycje","komunikacja"]`) ||
		!strings.Contains(string(raw), `"article:tag":["Transport","Inwestycje"]`) || !strings.Contains(string(raw), `"h-entry"`) ||
		strings.Count(string(raw), `"captureId"`) != 1 {
		t.Fatalf("keyword filter: %s", raw)
	}
	if resp, raw := api(t, "GET", e.srv.URL+"/api/facets", key, "", nil); resp.StatusCode != 200 || !strings.Contains(string(raw), `"name":"wiadomosci"`) {
		t.Fatalf("facets: %s", raw)
	}

	// Keys only read the API.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/urls"}, {"GET", "/api/sources"}, {"GET", "/"}, {"GET", "/archive"},
		{"GET", "/system/keys"}, {"POST", "/system/keys"},
	} {
		if resp, _ := api(t, c.method, e.srv.URL+c.path, key, "application/json", strings.NewReader("{}")); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s with an API key: %d", c.method, c.path, resp.StatusCode)
		}
	}

	keys, _ := e.auth.APIKeys(ctx)
	if len(keys) != 1 || keys[0].LastUsed.IsZero() || keys[0].CreatedBy != "admin" {
		t.Fatalf("%+v", keys)
	}
	_, page := b.get(t, "/system/keys")
	if strings.Contains(page, key) || !strings.Contains(page, "dashboard") {
		t.Fatal("key list must not show the secret")
	}
	b.post(t, fmt.Sprintf("/system/keys/%d/delete", keys[0].ID), url.Values{"csrf": {b.csrf}})
	if resp, _ := api(t, "GET", e.srv.URL+"/api/search", key, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("revoked key accepted")
	}

	viewer, _ := e.auth.CreateUser(ctx, "viewer", "viewer-password", auth.RoleViewer)
	vtok, _ := e.auth.CreateToken(ctx, viewer, "ro")
	if resp, _ := api(t, "GET", e.srv.URL+"/system/keys", vtok, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatal("viewer must not manage API keys")
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/api/pages", vtok, "", nil); resp.StatusCode != 200 {
		t.Fatal("user tokens still read the API")
	}
}

// TestOpenAPIMatchesRoutes calls every operation of docs/openapi.yaml with an
// API key and checks the required fields of a page.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var spec struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	ctx := context.Background()
	editor, _ := e.auth.CreateUser(ctx, "editor", "editor-password", auth.RoleEditor)
	etok, _ := e.auth.CreateToken(ctx, editor, "ci")
	api(t, "POST", e.srv.URL+"/api/urls", etok, "application/json", strings.NewReader(`{"url":"`+e.site.URL+`/news/article.html"}`))
	e.drain()
	ep := epoch.Of(time.Now())
	if _, err := e.coord.Seal(ctx, "", ep, true); err != nil {
		t.Fatal(err)
	}
	key, _ := e.auth.CreateAPIKey(ctx, "spec", "admin", time.Time{})
	_, raw := api(t, "GET", e.srv.URL+"/api/pages", key, "", nil)
	var list struct{ Results []map[string]any }
	json.Unmarshal(raw, &list)
	if len(list.Results) == 0 {
		t.Fatalf("no pages: %s", raw)
	}
	pg := list.Results[0]
	for _, f := range spec.Components.Schemas["Page"].Required {
		if _, ok := pg[f]; !ok {
			t.Errorf("page lacks required field %q", f)
		}
	}
	id := pg["captureId"].(string)
	links := pg["links"].(map[string]any)
	values := strings.NewReplacer("{id}", id, "{file}", id+".html",
		"{recordId}", strings.TrimPrefix(links["record"].(string), "/api/record/"),
		"{storage}", "default", "{path}", strings.TrimPrefix(ep.VolumeKey(1), "weekly/"))
	if len(spec.Paths) < 8 {
		t.Fatalf("paths: %v", spec.Paths)
	}
	for path, ops := range spec.Paths {
		for method := range ops {
			resp, body := api(t, strings.ToUpper(method), e.srv.URL+values.Replace(path), key, "", nil)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s %s: %d %.200s", method, path, resp.StatusCode, body)
			}
		}
	}
	if resp, body := api(t, "GET", e.srv.URL+"/api/openapi.yaml", "", "", nil); resp.StatusCode != 200 || !strings.HasPrefix(string(body), "openapi: 3.1") {
		t.Fatalf("served spec: %d", resp.StatusCode)
	}
}
