// Package views renders HTML templates (stdlib html/template) with htmx support.
//
// Layout convention:
//   - web/templates/layouts/base.html   : public shell (defines "base", calls "content")
//   - web/templates/layouts/app.html    : dashboard shell with sidebar (defines "app", calls "content")
//   - web/templates/pages/*.html        : page bodies (each defines "content")
//   - web/templates/partials/*.html     : htmx fragments (no layout)
//   - web/templates/shared/*.html     : icons and shared snippets
//
// Handlers render full pages for normal navigations and partials when
// HX-Request: true is present.
package views

import (
	"html/template"
	"net/http"
	"path"
	"reflect"
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
		"title": func(s string) string {
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
	public   map[string]*template.Template
	app      map[string]*template.Template
	partials map[string]*template.Template
	shared   map[string]any
}

// New parses the public layout and the dashboard layout together with each
// page. publicPages use base.html, appPages use app.html. Shared holds global
// template data (app name, htmx/swagger versions, base URL, env).
func New(shared map[string]any) (*Views, error) {
	if shared == nil {
		shared = map[string]any{}
	}
	v := &Views{
		public:   map[string]*template.Template{},
		app:      map[string]*template.Template{},
		partials: map[string]*template.Template{},
		shared:   shared,
	}

	base := template.New("").Funcs(funcMap())
	publicPages := []string{"index.html", "login.html"}
	for _, p := range publicPages {
		tmpl, err := template.Must(base.Clone()).ParseFS(webassets.TemplatesFS,
			"templates/shared/icons.html",
			"templates/components/alert.tmpl",
			"templates/components/button.tmpl",
			"templates/components/sidebar.tmpl",
			"templates/components/tabs.tmpl",
			"templates/layouts/base.html",
			path.Join("templates/pages", p),
		)
		if err != nil {
			return nil, err
		}
		v.public[p] = tmpl
	}

	appPages := []string{
		"dashboard.html", "connect.html", "contacts.html",
		"campaigns.html", "campaign-detail.html", "analytics.html", "templates.html",
		"settings.html",
	}
	for _, p := range appPages {
		tmpl, err := template.Must(base.Clone()).ParseFS(webassets.TemplatesFS,
			"templates/shared/icons.html",
			"templates/components/alert.tmpl",
			"templates/components/button.tmpl",
			"templates/components/sidebar.tmpl",
			"templates/components/tabs.tmpl",
			"templates/layouts/app.html",
			path.Join("templates/pages", p),
		)
		if err != nil {
			return nil, err
		}
		v.app[p] = tmpl
	}
	return v, nil
}

// RenderPage renders a public page inside the base layout.
// page is e.g. "index.html".
func (v *Views) RenderPage(w http.ResponseWriter, page string, data map[string]any) {
	tmpl, ok := v.public[page]
	if !ok {
		// Fall through to app pages so callers don't need to know the layout.
		v.RenderApp(w, page, data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	merged := merge(v.shared, data)
	merged["Page"] = page
	if err := tmpl.ExecuteTemplate(w, "base", merged); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// RenderApp renders a dashboard page inside the app (sidebar) layout.
// page is e.g. "dashboard.html".
func (v *Views) RenderApp(w http.ResponseWriter, page string, data map[string]any) {
	tmpl, ok := v.app[page]
	if !ok {
		if t, ok2 := v.public[page]; ok2 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			merged := merge(v.shared, data)
			merged["Page"] = page
			if err := t.ExecuteTemplate(w, "base", merged); err != nil {
				http.Error(w, "template error", http.StatusInternalServerError)
			}
			return
		}
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	merged := merge(v.shared, data)
	merged["Page"] = page
	if err := tmpl.ExecuteTemplate(w, "app", merged); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
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
