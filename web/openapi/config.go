package openapi

// Config is the operator-facing identity of the generated spec. Title
// and Version are required by OpenAPI 3.1 §4.5.2 (the Info object's
// mandatory fields); the remaining values are optional metadata folded
// into the document when set. Pass it to NewSpec, or, in the canonical
// path, set it on bootstrap.Wiring.OpenAPI and let the framework wire
// the spec + Swagger UI routes automatically.
type Config struct {
	Title       string
	Version     string
	Description string
	Servers     []Server
	Contact     *Contact
	License     *License

	// LanguageSelector opts the rendered Swagger UI into a global
	// language dropdown that injects Accept-Language on every "Try it
	// out" request. Default false. When true and at least one entry of
	// Languages is set, the UI page renders a <select> in its header and
	// a requestInterceptor that copies the selected value into the
	// Accept-Language header before the request leaves the browser.
	//
	// Under bootstrap.Run the Languages slice is populated automatically
	// from Wiring.Translations (dedup by configuration.Language, order of
	// declaration preserved) — operators just flip the flag. Consumers
	// that wire Register manually may set Languages by hand.
	LanguageSelector bool

	// Languages is the dropdown content surfaced by LanguageSelector.
	// Empty slice + LanguageSelector=true short-circuits to "no selector
	// rendered" rather than panicking, so a service with no Translations
	// registered still boots.
	Languages []LanguageOption
}

// LanguageOption is one entry of the Swagger UI language dropdown.
// Label is shown inside the <option> tag (visible to the operator);
// Value is what the requestInterceptor writes to the Accept-Language
// header — typically the framework's configuration.Language.HTTPPrefix
// (matches the framework's parseLanguage on the wire).
type LanguageOption struct {
	Label string
	Value string
}

// Server lists one entry under the OpenAPI document's `servers` block.
// URL is the base URL the rendered Swagger UI uses as the "try it out"
// target. Description is optional human-readable label.
type Server struct {
	URL         string
	Description string
}

// Contact is the maintainer's contact card on the Info object. Any
// subset of fields may be populated; empty fields are omitted from the
// rendered spec.
type Contact struct {
	Name  string
	Email string
	URL   string
}

// License is the rights statement on the Info object. Name is required
// by the OpenAPI spec when a License object is present; URL is
// optional.
type License struct {
	Name string
	URL  string
}
