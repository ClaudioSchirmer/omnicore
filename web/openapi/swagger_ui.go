package openapi

import (
	"html"
	"strings"
)

// SwaggerUIHTML returns the standalone HTML page served at the UI route.
// The page loads Swagger UI from the unpkg.com CDN and configures it to
// fetch the spec from specPath on the same origin.
//
// Going through a CDN keeps the framework's binary footprint small
// (versus embedding the ~1.5 MB swagger-ui-dist bundle in an
// embed.FS). Consumers that need fully offline operation (air-gapped
// environments, strict CSP, no external network) can override the UI
// route after Register has run — last write wins on the Fiber router —
// and serve their own HTML pointing at specPath.
//
// The <head> declares three inline icon links so browsers stop probing
// the conventional fallback paths (/favicon.ico, /apple-touch-icon.png,
// /apple-touch-icon-precomposed.png) — every miss otherwise lands on
// the framework's ErrorHandler and emits a RouteNotFoundNotification
// per asset. The favicon is an inline SVG (green Swagger square with
// "{}") so it scales crisply at any density; the apple-touch entries
// point at the empty data URI ("data:,"), telling Safari/iOS not to
// fetch anything.
//
// title escapes via html.EscapeString so an operator-supplied
// Config.Title cannot inject script into the page. An empty title
// falls back to "API Docs". specPath is rendered verbatim into a JS
// string literal — callers must pass a framework-controlled value
// (Register injects the canonical "/openapi.json"); no escaping is
// performed because the path is never operator input.
//
// languages drives the optional language dropdown. A nil/empty slice
// renders the page without the selector. A non-empty slice renders a
// <select> in the header (default value = languages[0].Value) and a
// requestInterceptor that copies the selected value to the
// Accept-Language header before every "Try it out" call. Both Label
// and Value are HTML-escaped because Languages is operator-controlled
// data routed through bootstrap from Wiring.Translations.
func SwaggerUIHTML(title, specPath string, languages []LanguageOption) string {
	safeTitle := html.EscapeString(title)
	if safeTitle == "" {
		safeTitle = "API Docs"
	}

	selectorCSS, selectorHTML, interceptorJS := renderLanguageSelector(languages)

	return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>` + safeTitle + `</title>
  <link rel="icon" type="image/svg+xml" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><rect width='16' height='16' rx='3' fill='%2385EA2D'/><text x='8' y='12' font-family='monospace' font-size='10' font-weight='bold' text-anchor='middle' fill='%23000'>{}</text></svg>">
  <link rel="apple-touch-icon" href="data:,">
  <link rel="apple-touch-icon-precomposed" href="data:,">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>html, body { margin: 0; padding: 0; } #swagger-ui { max-width: 1240px; margin: 0 auto; }` + selectorCSS + `</style>
</head>
<body>
` + selectorHTML + `  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-standalone-preset.js"></script>
  <script>
    window.onload = function() {
      window.ui = SwaggerUIBundle({
        url: '` + specPath + `',
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        layout: 'BaseLayout'` + interceptorJS + `
      });
    };
  </script>
</body>
</html>`
}

// renderLanguageSelector returns three fragments to splice into the
// page: (1) extra CSS for the language bar, appended inside the existing
// <style>; (2) the visible bar markup placed at the top of <body>; (3)
// the requestInterceptor key, comma-prefixed so it splices cleanly into
// the SwaggerUIBundle({...}) literal. All three are empty strings when
// languages is nil/empty so the page is byte-identical to the
// no-selector baseline (preserves the existing test guarantees around
// title escaping and spec path injection).
func renderLanguageSelector(languages []LanguageOption) (selectorCSS, selectorHTML, interceptorJS string) {
	if len(languages) == 0 {
		return "", "", ""
	}

	selectorCSS = ` #omnicore-lang-bar { max-width: 1240px; margin: 0 auto; padding: 12px 20px 0 20px; display: flex; justify-content: flex-end; align-items: center; gap: 8px; font-family: sans-serif; font-size: 13px; color: #3b4151; } #omnicore-lang-bar label { font-weight: 600; } #omnicore-lang-bar select { padding: 4px 8px; border: 1px solid #d9d9d9; border-radius: 4px; background: #fff; font-size: 13px; }`

	var b strings.Builder
	b.WriteString("  <div id=\"omnicore-lang-bar\">\n")
	b.WriteString("    <label for=\"omnicore-lang-selector\">Accept-Language</label>\n")
	b.WriteString("    <select id=\"omnicore-lang-selector\">\n")
	for _, opt := range languages {
		b.WriteString("      <option value=\"")
		b.WriteString(html.EscapeString(opt.Value))
		b.WriteString("\">")
		b.WriteString(html.EscapeString(opt.Label))
		b.WriteString("</option>\n")
	}
	b.WriteString("    </select>\n")
	b.WriteString("  </div>\n")
	selectorHTML = b.String()

	// The interceptor is a leading comma + key so it splices cleanly
	// after `layout: 'BaseLayout'` without us touching the surrounding
	// lines. Static JS — no operator input reaches the script body.
	interceptorJS = `,
        requestInterceptor: function(request) {
          var sel = document.getElementById('omnicore-lang-selector');
          if (sel && sel.value) {
            request.headers['Accept-Language'] = sel.value;
          }
          return request;
        }`

	return selectorCSS, selectorHTML, interceptorJS
}
