package handlers

import (
	"net/http"
	"strings"

	"github.com/devstroop/wam/api"
)

// OpenAPIYAML serves the canonical OpenAPI document.
// Canonical: GET /api-docs/openapi.yaml. Alias: GET /openapi.yaml.
func OpenAPIYAML(w http.ResponseWriter, _ *http.Request) {
	data, err := api.Spec()
	if err != nil {
		http.Error(w, "openapi spec not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// APIDocs serves Swagger UI at /api-docs/ loading /api-docs/openapi.yaml.
// swaggerUIVersion is hardcoded in internal/server/server.go.
func APIDocs(swaggerUIVersion string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Redirect /api-docs -> /api-docs/ so relative URLs resolve.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>WAM API Docs</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css" />
<style>body{margin:0}.topbar{display:none}</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js"></script>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-standalone-preset.js"></script>
<script>
window.onload = () => {
  SwaggerUIBundle({
    url: "/api-docs/openapi.yaml",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
    layout: "StandaloneLayout",
    persistAuthorization: true,
  });
};
</script>
</body>
</html>`))
	}
}

// SwaggerRedirect permanently redirects legacy /swagger* to /api-docs/.
func SwaggerRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/api-docs/"
	if strings.HasPrefix(r.URL.Path, "/swagger/openapi") {
		target = "/api-docs/openapi.yaml"
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
