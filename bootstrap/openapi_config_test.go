package bootstrap

import (
	"strings"
	"testing"
)

func TestOpenAPIConfig_DefaultsWhenAbsent(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired) // no `openapi:` block
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.OpenAPI.UIPath != defaultOpenAPIUIPath {
		t.Errorf("UIPath default = %q, want %q", cfg.OpenAPI.UIPath, defaultOpenAPIUIPath)
	}
	if cfg.OpenAPI.RootRedirect {
		t.Errorf("RootRedirect default = true, want false")
	}
}

func TestOpenAPIConfig_CustomValuesParsed(t *testing.T) {
	yml := validYAMLAllRequired + `openapi:
  uiPath: /swagger
  rootRedirect: true
`
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.OpenAPI.UIPath != "/swagger" {
		t.Errorf("UIPath = %q, want %q", cfg.OpenAPI.UIPath, "/swagger")
	}
	if !cfg.OpenAPI.RootRedirect {
		t.Errorf("RootRedirect = false, want true")
	}
}

func TestOpenAPIConfig_ValidateRejectsRelativePath(t *testing.T) {
	yml := validYAMLAllRequired + `openapi:
  uiPath: docs
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("expected error for uiPath without leading slash")
	}
	if !strings.Contains(err.Error(), "must start with") {
		t.Errorf("error should mention the leading-slash rule; got: %v", err)
	}
}

func TestOpenAPIConfig_ValidateRejectsSpecPathCollision(t *testing.T) {
	yml := validYAMLAllRequired + `openapi:
  uiPath: /openapi.json
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("expected error for uiPath colliding with spec route")
	}
	if !strings.Contains(err.Error(), "spec route") {
		t.Errorf("error should mention the spec-route collision; got: %v", err)
	}
}

func TestOpenAPIConfig_ValidateRejectsHealthCollision(t *testing.T) {
	yml := validYAMLAllRequired + `openapi:
  uiPath: /readyz
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("expected error for uiPath colliding with a health-probe route")
	}
	if !strings.Contains(err.Error(), "health-probe route") {
		t.Errorf("error should mention the health-probe-route collision; got: %v", err)
	}
}
