// Package views renders HTML templates (stdlib html/template) with htmx support.
//
// Layout convention:
//   - web/templates/layouts/base.html   : public shell (defines "base", calls "content")
//   - web/templates/layouts/app.html    : dashboard shell with sidebar (defines "app", calls "content")
//   - web/templates/layouts/admin.html  : admin shell with admin nav (defines "admin", calls "content")
//   - web/templates/pages/*.html        : page bodies (each defines "content")
//   - web/templates/partials/*.html     : htmx fragments (no layout)
//   - web/templates/shared/*.html     : icons and shared snippets
//
// Layout binding: a page may declare its layout with a tag on its first
// line, e.g. {{/* layout: admin */}} (Blazor-style @layout). The tag wins;
// otherwise the page keeps its registration fallback (publicPages → base,
// appPages → app, adminPages → admin), so untagged pages behave exactly as
// before. Unknown layout names fail New() fast, before serving.
//
// Handlers render full pages for normal navigations and partials when
// HX-Request: true is present.
package views

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	webassets "github.com/devstroop/wam/web"
)

func funcMap() template.FuncMap {
	return template.FuncMap{
		"dict": func(values ...any) map[string]any {
			m := make(map[string]any, len(values)/2)
			for i := 0; i+1 < len(values); i += 2 {
				if k, ok := values[i].(string); ok {
					m[k] = values[i+1]
				}
			}
			return m
		},
		"deref": func(v any) any {
			rv := reflect.ValueOf(v)
			if rv.Kind() == reflect.Ptr && !rv.IsNil() {
				return rv.Elem().Interface()
			}
			return v
		},
		"htmlSafe": func(s string) template.HTML { return template.HTML(s) },
		"title": func(v any) string {
			s, _ := v.(string)
			if s == "" {
				return s
			}
			// Title-case each word, preserve rest lowercased for Pascal
			words := strings.Fields(s)
			for i, w := range words {
				if len(w) == 0 {
					continue
				}
				runes := []rune(strings.ToLower(w))
				runes[0] = unicode.ToUpper(runes[0])
				words[i] = string(runes)
			}
			return strings.Join(words, " ")
		},
		"truncate": func(s string, n int) string {
			if n <= 0 || len(s) <= n {
				return s
			}
			if n <= 3 {
				return s[:n]
			}
			return s[:n-3] + "…"
		},
	}
}

// Views holds parsed templates and shared template data.
type Views struct {
	partials map[string]*template.Template
	shared   map[string]any
	// pages records each page's parsed template and resolved layout root
	// (layout file base name, e.g. "admin" for templates/layouts/admin.html).
	pages map[string]pageEntry
}

type pageEntry struct {
	tmpl   *template.Template
	layout string
}

// layoutDirective matches a page-declared layout tag, e.g.
// {{/* layout: admin */}}. Only the file head is scanned.
var layoutDirective = regexp.MustCompile(`\{\{/\*\s*layout:\s*([a-z]+)\s*\*/\}\}`)

// parseLayoutTag extracts a layout name from raw page bytes ("", false when
// untagged). Pure function — the registry handles FS reads + validation.
func parseLayoutTag(raw []byte) (string, bool) {
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	m := layoutDirective.FindSubmatch(head)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// layoutOfPage resolves a page's layout: explicit tag wins, else fallback.
// The tag is valid when templates/layouts/<name>.html exists; the shell
// must define a root template of the same name (checked at parse time).
// Unknown layouts error (fail fast at boot, before serving).
func layoutOfPage(page, fallback string) (string, error) {
	raw, err := webassets.TemplatesFS.ReadFile(path.Join("templates/pages", page))
	if err != nil {
		return "", err
	}
	name, tagged := parseLayoutTag(raw)
	if !tagged {
		return fallback, nil
	}
	if _, err := fs.Stat(webassets.TemplatesFS, path.Join("templates/layouts", name+".html")); err != nil {
		return "", fmt.Errorf("views: %s: unknown layout %q", page, name)
	}
	return name, nil
}

// New parses the public layout and the dashboard layout together with each
// page. publicPages use base.html, appPages use app.html, adminPages use
// admin.html. Shared holds global template data (app name, htmx/swagger
// versions, base URL, env).
func New(shared map[string]any) (*Views, error) {
	if shared == nil {
		shared = map[string]any{}
	}
	v := &Views{
		partials: map[string]*template.Template{},
		shared:   shared,
		pages:    map[string]pageEntry{},
	}

	base := template.New("").Funcs(funcMap())
	sharedIncludes := []string{
		"templates/shared/icons.html",
		"templates/components/alert.tmpl",
		"templates/components/button.tmpl",
		"templates/components/sidebar.tmpl",
		"templates/components/tabs.tmpl",
	}
	// register parses pages with their resolved layout. extra lists
	// page-specific includes. The resolved layout decides the shell
	// (team.tmpl ships with app/admin shells); a missing root define in
	// the shell fails here, at boot.
	register := func(pages []string, fallback string, extra []string) error {
		for _, p := range pages {
			layout, err := layoutOfPage(p, fallback)
			if err != nil {
				return err
			}
			files := append(append([]string{}, sharedIncludes...), extra...)
			files = append(files, "templates/layouts/"+layout+".html", path.Join("templates/pages", p))
			tmpl, err := template.Must(base.Clone()).ParseFS(webassets.TemplatesFS, files...)
			if err != nil {
				return err
			}
			if tmpl.Lookup(layout) == nil {
				return fmt.Errorf("views: %s: layout %q defines no %q root template", p, layout, layout)
			}
			v.pages[p] = pageEntry{tmpl: tmpl, layout: layout}
		}
		return nil
	}

	publicPages := []string{"index.html", "auth/login.html", "auth/signup.html", "auth/verify.html", "auth/forgot.html", "auth/reset.html", "invite-accept.html"}
	if err := register(publicPages, "base", nil); err != nil {
		return nil, err
	}

	appPages := []string{
		"dashboard.html", "connect.html", "contacts.html",
		"campaigns.html", "campaign-detail.html", "analytics.html", "templates.html",
		"accounts.html", "settings.html",
	}
	if err := register(appPages, "app", []string{"templates/components/team.tmpl"}); err != nil {
		return nil, err
	}

	adminPages := []string{
		"admin/overview.html",
		"admin/users.html",
		"admin/roles.html",
		"admin/grants.html",
		"admin/accounts.html",
		"admin/keys.html",
		"admin/webhooks.html",
		"admin/billing.html",
		"admin/settings.html",
	}
	if err := register(adminPages, "admin", []string{"templates/components/team.tmpl"}); err != nil {
		return nil, err
	}
	return v, nil
}

// LayoutOf reports a page's resolved layout.
func (v *Views) LayoutOf(page string) (string, bool) {
	e, ok := v.pages[page]
	if !ok {
		return "", false
	}
	return e.layout, true
}

// Render renders a page inside its resolved layout (page-declared tag wins,
// else the registration fallback). Unknown pages 404.
func (v *Views) Render(w http.ResponseWriter, page string, data map[string]any) {
	e, ok := v.pages[page]
	if !ok {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	merged := merge(v.shared, data)
	merged["Page"] = page
	if err := e.tmpl.ExecuteTemplate(w, e.layout, merged); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// RenderPage renders a public page inside the base layout.
// page is e.g. "index.html". Delegates to Render.
func (v *Views) RenderPage(w http.ResponseWriter, page string, data map[string]any) {
	v.Render(w, page, data)
}

// RenderApp renders a dashboard page inside the app (sidebar) layout.
// page is e.g. "dashboard.html". Delegates to Render.
func (v *Views) RenderApp(w http.ResponseWriter, page string, data map[string]any) {
	v.Render(w, page, data)
}

// RenderAdmin renders an admin page inside the admin layout.
// page is e.g. "admin/overview.html". Delegates to Render.
func (v *Views) RenderAdmin(w http.ResponseWriter, page string, data map[string]any) {
	v.Render(w, page, data)
}

// RenderPartial renders a single partial file (htmx fragment).
// name is the file base, e.g. "contact-rows" for partials/contact-rows.html.
func (v *Views) RenderPartial(w http.ResponseWriter, name string, data map[string]any) {
	tmpl, ok := v.partials[name]
	if !ok {
		parsed, err := template.New("").Funcs(funcMap()).ParseFS(webassets.TemplatesFS,
			"templates/shared/icons.html",
			"templates/components/alert.tmpl",
			"templates/components/button.tmpl",
			"templates/components/sidebar.tmpl",
			"templates/components/tabs.tmpl",
			path.Join("templates/partials", name+".html"))
		if err != nil {
			http.Error(w, "partial not found", http.StatusInternalServerError)
			return
		}
		tmpl = parsed
		v.partials[name] = parsed
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The partial file itself is named "templates/partials/<name>.html" after ParseFS.
	// Execute that specific template, not the empty root.
	target := path.Join("templates/partials", name+".html")
	if err := tmpl.ExecuteTemplate(w, target, merge(v.shared, data)); err != nil {
		// Fallback: try bare filename for older Go versions
		if err2 := tmpl.ExecuteTemplate(w, name+".html", merge(v.shared, data)); err2 != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
		}
	}
}

func merge(shared, data map[string]any) map[string]any {
	out := make(map[string]any, len(shared)+len(data))
	for k, val := range shared {
		out[k] = val
	}
	for k, val := range data {
		out[k] = val
	}
	return out
}
