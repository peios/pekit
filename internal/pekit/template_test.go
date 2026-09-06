package pekit

import (
	"strings"
	"testing"
)

// A version pekit's own model does not describe — a peipkg version
// carrying an epoch or a tilde, which its regex rejects outright —
// reaches a template through mustParseVersion, which swallows the parse
// error and returns a Version with only Raw set. Every derived token
// then rendered as an empty string with no diagnostic anywhere: a
// publish destination silently missing its major number (PEI-422).
func TestRenderTemplateRefusesDerivedTokensOfAnUndecomposableVersion(t *testing.T) {
	v := mustParseVersion("2:0.5.0-1") // epoch: pekit's regex rejects it
	if v.Parsed {
		t.Fatal("the fixture parsed after all; pick a version the model really rejects")
	}

	for _, token := range []string{"{{major}}", "{{minor}}", "{{patch}}",
		"{{prerelease}}", "{{buildmeta}}"} {
		out, err := RenderTemplate("pkg-"+token, TemplateContext{Version: v})
		if err == nil {
			t.Errorf("RenderTemplate(%s) = %q, want a diagnostic rather than an empty render",
				token, out)
			continue
		}
		if !strings.Contains(err.Error(), "does not decompose") {
			t.Errorf("RenderTemplate(%s) error = %v, want it to say why", token, err)
		}
	}

	// {{version}} is still available: Raw is the one field that survives,
	// and a recipe using only it is not broken by the version being one
	// pekit cannot take apart.
	out, err := RenderTemplate("pkg-{{version}}.peipkg", TemplateContext{Version: v})
	if err != nil {
		t.Fatalf("RenderTemplate({{version}}): %v", err)
	}
	if out != "pkg-2:0.5.0-1.peipkg" {
		t.Errorf("RenderTemplate({{version}}) = %q", out)
	}
}

// The ordinary case is untouched.
func TestRenderTemplateStillRendersADecomposableVersion(t *testing.T) {
	v := mustParseVersion("1.26.2")
	if !v.Parsed {
		t.Fatal("1.26.2 should parse")
	}
	out, err := RenderTemplate("nginx-{{major}}.{{minor}}/{{version}}", TemplateContext{Version: v})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out != "nginx-1.26/1.26.2" {
		t.Errorf("RenderTemplate = %q", out)
	}
}

func TestRenderTemplatePreservesArbitraryNumericCoreAndFirstThreeCompatibility(t *testing.T) {
	v := mustParseVersion("0.5.13.10")
	out, err := RenderTemplate("dash-{{version}}/{{major}}.{{minor}}.{{patch}}", TemplateContext{Version: v})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out != "dash-0.5.13.10/0.5.13" {
		t.Errorf("RenderTemplate = %q", out)
	}
}
