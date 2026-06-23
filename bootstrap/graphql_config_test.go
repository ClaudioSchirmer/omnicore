package bootstrap

import (
	"strings"
	"testing"
)

func TestGraphQLConfig_DefaultsWhenAbsent(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired) // no `graphql:` block
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.GraphQL.Path != defaultGraphQLPath {
		t.Errorf("Path default = %q, want %q", cfg.GraphQL.Path, defaultGraphQLPath)
	}
	if cfg.GraphQL.UIPath != defaultGraphQLUIPath {
		t.Errorf("UIPath default = %q, want %q", cfg.GraphQL.UIPath, defaultGraphQLUIPath)
	}
	if cfg.GraphQL.RootRedirect || cfg.GraphQL.Playground || cfg.GraphQL.Introspection {
		t.Errorf("toggles must default off, got %+v", cfg.GraphQL)
	}
}

func TestGraphQLConfig_PlaygroundUIPathCollisionRejected(t *testing.T) {
	yml := validYAMLAllRequired + `graphql:
  playground: true
  uiPath: /docs
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected uiPath collision error, got: %v", err)
	}
}

func TestGraphQLConfig_CustomValuesParsed(t *testing.T) {
	yml := validYAMLAllRequired + `graphql:
  path: /gql
  rootRedirect: true
`
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.GraphQL.Path != "/gql" {
		t.Errorf("Path = %q, want /gql", cfg.GraphQL.Path)
	}
	if !cfg.GraphQL.RootRedirect {
		t.Errorf("RootRedirect = false, want true")
	}
}

func TestGraphQLConfig_ValidateRejectsRelativePath(t *testing.T) {
	yml := validYAMLAllRequired + `graphql:
  path: gql
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("expected leading-slash error, got: %v", err)
	}
}

func TestGraphQLConfig_ValidateRejectsFrameworkRouteCollision(t *testing.T) {
	yml := validYAMLAllRequired + `graphql:
  path: /health
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected framework-route collision error, got: %v", err)
	}
}

