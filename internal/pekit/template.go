package pekit

import (
	"fmt"
	"regexp"
)

type TemplateContext struct {
	Version   Version
	Multipack string
}

var templateRE = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

func RenderTemplate(raw string, ctx TemplateContext) (string, error) {
	var firstErr error
	out := templateRE.ReplaceAllStringFunc(raw, func(token string) string {
		if firstErr != nil {
			return ""
		}
		name := templateRE.FindStringSubmatch(token)[1]
		if name == "multipack" {
			if ctx.Multipack == "" {
				firstErr = fmt.Errorf("{{multipack}} is not available in this context")
				return ""
			}
			return ctx.Multipack
		}
		vars := ctx.Version.TemplateVars()
		value, ok := vars[name]
		if !ok {
			firstErr = fmt.Errorf("unknown template variable %q", name)
			return ""
		}
		if name == "version" && value == "" {
			firstErr = fmt.Errorf("{{version}} is not available without a selected version")
			return ""
		}
		if value == "" && (name == "minor" || name == "patch") {
			firstErr = fmt.Errorf("{{%s}} is not available for version %q", name, ctx.Version.Raw)
			return ""
		}
		return value
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}
