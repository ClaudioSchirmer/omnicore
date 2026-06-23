package graphql

import (
	"html"

	"github.com/gofiber/fiber/v3"
)

// Playground returns a fiber.Handler serving a GraphiQL HTML page wired to the
// GraphQL endpoint at `endpoint`. It mirrors the OpenAPI Swagger-UI posture:
// a static page loading the UI bundle from a CDN (override the route after
// mount for offline operation). Autocomplete / schema docs require
// introspection to be enabled on the registry.
func (r *Registry) Playground(endpoint string) fiber.Handler {
	page := graphiqlHTML(endpoint)
	return func(c fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
		return c.SendString(page)
	}
}

// graphiqlHTML renders the GraphiQL page targeting endpoint. endpoint is HTML-
// escaped because it originates from operator config; the rest of the page is
// static, so no other interpolation reaches the document.
func graphiqlHTML(endpoint string) string {
	safe := html.EscapeString(endpoint)
	// Versions are PINNED: React's UMD builds (umd/react.production.min.js) were
	// removed in React 19, so an unversioned `react`/`graphiql` resolves to a
	// build with no UMD bundle and the page fails to load GraphiQL. GraphiQL 3.x
	// + React 18 UMD is the combination this `GraphiQL.createFetcher` +
	// `ReactDOM.render` pattern targets.
	return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <title>GraphQL</title>
  <style>body{margin:0}#graphiql{height:100vh}</style>
  <link rel="stylesheet" href="https://unpkg.com/graphiql@3/graphiql.min.css"/>
</head>
<body>
  <div id="graphiql">Loading…</div>
  <script src="https://unpkg.com/react@18/umd/react.production.min.js"></script>
  <script src="https://unpkg.com/react-dom@18/umd/react-dom.production.min.js"></script>
  <script src="https://unpkg.com/graphiql@3/graphiql.min.js"></script>
  <script>
    const fetcher = GraphiQL.createFetcher({ url: '` + safe + `' });
    ReactDOM.render(
      React.createElement(GraphiQL, { fetcher }),
      document.getElementById('graphiql'),
    );
  </script>
</body>
</html>`
}
