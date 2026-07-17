package pekit

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

type Command string

const (
	CommandBuild     Command = "build"
	CommandTest      Command = "test"
	CommandInstall   Command = "install"
	CommandClean     Command = "clean"
	CommandPackage   Command = "package"
	CommandPublish   Command = "publish"
	CommandGen       Command = "gen"
	CommandVerify    Command = "verify"
	CommandWorkspace Command = "workspace"
)

var commands = map[string]Command{
	"build":     CommandBuild,
	"test":      CommandTest,
	"install":   CommandInstall,
	"clean":     CommandClean,
	"package":   CommandPackage,
	"publish":   CommandPublish,
	"gen":       CommandGen,
	"verify":    CommandVerify,
	"workspace": CommandWorkspace,
}

type Invocation struct {
	Args []string
	Cwd  string

	Command         Command
	WorkspaceMode   bool
	DelegateCommand Command

	RecipeFlag      string
	WorkspaceFlag   string
	RemoteRecipe    string
	Positionals     []string
	AfterDoubleDash []string

	AllowUnused bool
	Suppressed  []string
	DryRun      bool
	Quiet       bool
	Verbose     bool
	JSON        bool

	FailFast bool
	Jobs     int

	Version                    string
	Latest                     bool
	AllVersions                bool
	SuppressUnsupportedVersion bool

	Local       *string
	PreferLocal *string

	NoBuild            *string
	NoVerify           *string
	EnvName            string
	Keyrings           []string
	KeyringValues      map[string]string
	ResolvedKeyringEnv map[string]string
	RefreshSource      bool
	AllowUnanchored    bool

	All        bool
	OutputOnly bool
	TargetOnly bool
}

func (i Invocation) EffectiveCommand() Command {
	if i.WorkspaceMode {
		return i.DelegateCommand
	}
	return i.Command
}

func (i Invocation) selectors() []string {
	out := append([]string(nil), i.Positionals...)
	out = append(out, i.AfterDoubleDash...)
	return out
}

type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
}

type Context struct {
	App             *App
	Inv             Invocation
	Renderer        Renderer
	Start           time.Time
	PublishRegistry *DestinationRegistry
}

type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Member  string `json:"member,omitempty"`
	Target  string `json:"target,omitempty"`
	Package string `json:"package,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

func (d Diagnostic) Error() string {
	if d.Path != "" {
		return fmt.Sprintf("%s: %s: %s", d.Code, d.Path, d.Message)
	}
	return fmt.Sprintf("%s: %s", d.Code, d.Message)
}

func diag(code, msg string, args ...any) error {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	return Diagnostic{Code: code, Message: msg}
}

func wrapDiag(code, msg string, err error) error {
	if err == nil {
		return Diagnostic{Code: code, Message: msg}
	}
	var d Diagnostic
	if errors.As(err, &d) {
		if d.Code == "" {
			d.Code = code
		}
		if msg != "" {
			d.Message = msg + ": " + d.Message
		}
		return d
	}
	if msg == "" {
		msg = err.Error()
	} else {
		msg = msg + ": " + err.Error()
	}
	return Diagnostic{Code: code, Message: msg}
}

func diagCode(err error) string {
	var d Diagnostic
	if errors.As(err, &d) {
		return d.Code
	}
	return ""
}

var selectorRE = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

func validateSelector(kind, name string) error {
	if name == "" {
		return diag("invalid_selector", "%s selector is empty", kind)
	}
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, "/:") || !selectorRE.MatchString(name) {
		return diag("invalid_selector", "%s selector %q is not a canonical v2 selector", kind, name)
	}
	return nil
}

func ptrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}
