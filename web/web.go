// Package web embeds HTML templates and static assets for htmx pages.
package web

import "embed"

// TemplatesFS holds web/templates (layouts, pages, partials, components).
//
//go:embed templates/* templates/layouts/* templates/pages/* templates/partials/* templates/components/* templates/shared/*
var TemplatesFS embed.FS

// StaticFS holds web/static (css, js).
//
//go:embed static/* static/css/* static/js/*
var StaticFS embed.FS
