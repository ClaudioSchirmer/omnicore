package bootstrap

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/read/mongo"
)

func TestBuildViewMaxLimitResolver_OverrideWinsOverYamlDefault(t *testing.T) {
	views := []*mongo.ViewDefinition{
		mongo.View("capped").Root("capped").Version(1).MaxLimit(25),
		mongo.View("plain").Root("plain").Version(1), // no per-view override
	}
	resolve := buildViewMaxLimitResolver(views, 200)

	if got := resolve("capped"); got != 25 {
		t.Errorf("per-view override must win: resolve(capped) = %d, want 25", got)
	}
	if got := resolve("plain"); got != 200 {
		t.Errorf("no override falls back to yaml default: resolve(plain) = %d, want 200", got)
	}
	if got := resolve("unknown"); got != 200 {
		t.Errorf("unknown view falls back to yaml default: resolve(unknown) = %d, want 200", got)
	}
}

func TestBuildViewMaxLimitResolver_ZeroOverrideIgnored(t *testing.T) {
	// MaxLimitValue() == 0 means "no override" — the resolver must not
	// register it, so the yaml default applies.
	views := []*mongo.ViewDefinition{
		mongo.View("v").Root("v").Version(1), // MaxLimit unset → 0
	}
	resolve := buildViewMaxLimitResolver(views, 0)
	if got := resolve("v"); got != 0 {
		t.Errorf("resolve(v) = %d, want 0 (delegate to framework default)", got)
	}
}
