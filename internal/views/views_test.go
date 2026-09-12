package views

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseLayoutTag covers the page-declared layout directive: spacing
// variants, head-only scanning, and non-matches.
func TestParseLayoutTag(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"plain tag", "{{/* layout: admin */}}\n{{define \"content\"}}", "admin", true},
		{"extra spaces", "{{/*layout:app*/}}", "app", true},
		{"tabs", "{{/*\tlayout:\tbase\t*/}}", "base", true},
		{"no tag", "{{define \"content\"}}hi", "", false},
		{"lookalike text", "layout: admin without comment", "", false},
		{"uppercase name", "{{/* layout: Admin */}}", "", false},
		{"beyond head", strings.Repeat("x", 600) + "{{/* layout: admin */}}", "", false},
	}
	for _, tc := range cases {
		got, ok := parseLayoutTag([]byte(tc.raw))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLayoutTagWins proves the directive beats the fallback: overview.html
// is tagged admin, so even a wrong fallback resolves admin.
func TestLayoutTagWins(t *testing.T) {
	got, err := layoutOfPage("admin/overview.html", "app")
	if err != nil {
		t.Fatalf("layoutOfPage: %v", err)
	}
	if got != "admin" {
		t.Fatalf("got %q, want admin (tag must win)", got)
	}
	if _, err := layoutOfPage("nope.html", "app"); err == nil {
		t.Fatal("missing page should error")
	}
}

// TestPageLayouts pins every page's resolved layout: explicit
// {{/* layout: X */}} tags win, else the registration fallback applies.
// Any page move or regroup that changes resolution fails here on purpose.
func TestPageLayouts(t *testing.T) {
	v, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := map[string]string{
		// auth fallback (publicPages) — all tagged auth in production files
		"index.html":      "base",
		"auth/login.html": "auth", "auth/signup.html": "auth",
		"auth/verify.html": "auth", "auth/forgot.html": "auth", "auth/reset.html": "auth",
		"invite-accept.html": "auth",
		// app fallback (appPages)
		"dashboard.html": "app", "connect.html": "app", "contacts.html": "app",
		"campaigns.html": "app", "campaign-detail.html": "app", "analytics.html": "app",
		"templates.html": "app", "accounts.html": "app", "settings.html": "app",
		// admin fallback (adminPages)
		"admin/overview.html": "admin", "admin/users.html": "admin", "admin/roles.html": "admin",
		"admin/grants.html": "admin", "admin/accounts.html": "admin", "admin/keys.html": "admin",
		"admin/webhooks.html": "admin", "admin/billing.html": "admin",
		"admin/settings.html": "admin",
	}
	for page, wantLayout := range want {
		got, ok := v.LayoutOf(page)
		if !ok {
			t.Errorf("%s: not registered", page)
			continue
		}
		if got != wantLayout {
			t.Errorf("%s: layout = %q, want %q", page, got, wantLayout)
		}
	}
	if _, ok := v.LayoutOf("nope.html"); ok {
		t.Error("unknown page resolved")
	}
	if len(v.pages) != len(want) {
		t.Errorf("registered %d pages, want %d (stale list?)", len(v.pages), len(want))
	}
}

// TestRenderUsesResolvedLayout renders one page per layout through the
// single Render entry and checks each shell's marker.
func TestRenderUsesResolvedLayout(t *testing.T) {
	v, err := New(map[string]any{
		"AppName": "WAM", "AppTagline": "T", "Env": "test",
		"BaseURL": "http://test", "HTMXVersion": "4.0.0",
		"SwaggerUIVersion": "5.17.14", "IsDev": false, "Year": 2026,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := map[string]string{
		"index.html":          "WAM",               // base shell brand
		"dashboard.html":      "sidebar-text",      // app shell nav
		"accounts.html":       "sidebar-text",      // app shell nav
		"admin/overview.html": "Back to Dashboard", // admin shell footer
		"admin/accounts.html": "Back to Dashboard", // admin shell footer
	}
	for page, marker := range cases {
		rec := httptest.NewRecorder()
		v.Render(rec, page, map[string]any{"Title": "T"})
		if rec.Code != 200 {
			t.Errorf("%s: status %d", page, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), marker) {
			t.Errorf("%s: missing shell marker %q", page, marker)
		}
	}
	rec := httptest.NewRecorder()
	v.Render(rec, "nope.html", nil)
	if rec.Code != 404 {
		t.Errorf("unknown page status = %d, want 404", rec.Code)
	}
}
