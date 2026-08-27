package pekit

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var execCommand = exec.Command

func runTarget(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, target TargetConfig, member string) error {
	stage := targetStage(source, target.Kind, target.Name)
	if shouldReuseBuild(ctx.Inv, target, stage) {
		ctx.Renderer.Event(Event{Type: "target_reuse", Member: member, Target: target.Name, Version: version.Raw, Message: "reusing staged build target"})
		return nil
	}
	if ctx.Inv.DryRun {
		ctx.Renderer.Event(Event{Type: "target_plan", Member: member, Target: target.Name, Version: version.Raw, Path: stage, Message: "would run target"})
		return nil
	}
	if target.ClearOut {
		if err := os.RemoveAll(stage); err != nil {
			return wrapDiag("clean_stage", stage, err)
		}
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return wrapDiag("mkdir", stage, err)
	}
	if ctx.Inv.Verbose {
		ctx.Renderer.Event(Event{Type: "target_stage", Member: member, Target: target.Name, Version: version.Raw, Path: stage, Message: "prepared target stage"})
	}
	env, err := BuildCommandEnv(ctx, recipe, workspace, source, version, target, stage)
	if err != nil {
		return err
	}
	cwd := source.SourceRoot
	if target.Kind == CommandClean {
		cwd = recipe.Root
	}
	start := time.Now()
	ctx.Renderer.Event(Event{Type: "target_start", Member: member, Target: target.Name, Version: version.Raw, Message: "running target"})
	if err := executeCommand(ctx, target.Command, env, cwd, member, version.Raw, string(target.Kind)+":"+target.Name); err != nil {
		return wrapDiag("target_failed", string(target.Kind)+":"+target.Name, err)
	}
	// Signing is the last mutation of the target's output: it runs after
	// the command has finished every strip/split/patch of its own, and
	// before any dependent target or package sees the files.
	if err := pipSignTarget(ctx, recipe, workspace, target, stage, member, version.Raw); err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "target_success", Member: member, Target: target.Name, Version: version.Raw, DurationMS: time.Since(start).Milliseconds(), Message: "target succeeded"})
	return nil
}

func targetStage(source SourceState, kind Command, name string) string {
	dir := string(kind)
	if kind == CommandBuild {
		dir = "build"
	}
	return filepath.Join(source.WorkBase, dir, name)
}

func shouldReuseBuild(inv Invocation, target TargetConfig, stage string) bool {
	if target.Kind != CommandBuild || inv.NoBuild == nil || !dirExists(stage) {
		return false
	}
	if *inv.NoBuild == "" {
		return true
	}
	for _, item := range strings.Split(*inv.NoBuild, ",") {
		if strings.TrimSpace(item) == target.Name {
			return true
		}
	}
	return false
}

func executeCommand(ctx *Context, command ShellCommand, env CommandEnv, cwd, member, version, target string) error {
	if command.Shell != "" {
		script := env.Script + "\n" + command.Shell
		if env.Wrap.Shell != "" {
			wrapped := strings.Replace(env.Wrap.Shell, "{{command}}", shellQuote(script), 1)
			return runProcess(ctx, cwd, member, version, target, "sh", []string{"-euc", wrapped}, env.Outer)
		}
		if len(env.Wrap.Args) > 0 {
			prog, args := renderArrayWrapper(env.Wrap.Args, script)
			return runProcess(ctx, cwd, member, version, target, prog, args, env.Outer)
		}
		return runProcess(ctx, cwd, member, version, target, "sh", []string{"-euc", script}, env.Outer)
	}
	if len(command.Args) > 0 {
		if env.Wrap.Shell != "" || len(env.Wrap.Args) > 0 {
			script := env.Script + "\nexec " + shellJoin(command.Args)
			if env.Wrap.Shell != "" {
				wrapped := strings.Replace(env.Wrap.Shell, "{{command}}", shellQuote(script), 1)
				return runProcess(ctx, cwd, member, version, target, "sh", []string{"-euc", wrapped}, env.Outer)
			}
			prog, args := renderArrayWrapper(env.Wrap.Args, script)
			return runProcess(ctx, cwd, member, version, target, prog, args, env.Outer)
		}
		return runProcess(ctx, cwd, member, version, target, command.Args[0], command.Args[1:], env.Values)
	}
	return fmt.Errorf("empty command")
}

func renderArrayWrapper(wrapper []string, script string) (string, []string) {
	args := make([]string, 0, len(wrapper)-1)
	for _, arg := range wrapper[1:] {
		if arg == "{{command}}" {
			args = append(args, script)
		} else {
			args = append(args, arg)
		}
	}
	return wrapper[0], args
}

func shellJoin(args []string) string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, shellQuote(arg))
	}
	return strings.Join(out, " ")
}

func runProcess(ctx *Context, cwd, member, version, target, prog string, args []string, env map[string]string) error {
	cmd := execCommand(prog, args...)
	cmd.Dir = cwd
	cmd.Env = envSlice(env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamOutput(ctx, stdout, member, version, target, "stdout")
	}()
	go func() {
		defer wg.Done()
		streamOutput(ctx, stderr, member, version, target, "stderr")
	}()
	wg.Wait()
	return cmd.Wait()
}

func streamOutput(ctx *Context, r io.Reader, member, version, target, stream string) {
	buf := make([]byte, 8192)
	var pending bytes.Buffer
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			pending.WriteString(chunk)
			text := pending.String()
			lastNL := strings.LastIndex(text, "\n")
			if lastNL >= 0 {
				emit := text[:lastNL+1]
				ctx.Renderer.Output(member, version, target, stream, emit)
				pending.Reset()
				pending.WriteString(text[lastNL+1:])
			}
		}
		if err != nil {
			if pending.Len() > 0 {
				ctx.Renderer.Output(member, version, target, stream, pending.String())
			}
			return
		}
	}
}

func exportScript(values map[string]string) string {
	keys := sortedKeys(values)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(shellQuote(values[key]))
		b.WriteByte('\n')
	}
	return b.String()
}

func exportScriptLayers(managed, keyring map[string]string, user []EnvVar) string {
	var b strings.Builder
	appendQuotedExports(&b, managed)
	appendQuotedExports(&b, keyring)
	for _, env := range user {
		b.WriteString("export ")
		b.WriteString(env.Name)
		b.WriteByte('=')
		b.WriteString(shellExpandable(env.Value))
		b.WriteByte('\n')
	}
	return b.String()
}

// shellExpandable renders a user env value as a double-quoted shell word.
//
// User env values are shell expressions, not literals: a layer may build on an
// earlier one (CFLAGS = "$CFLAGS -fPIC") or reference a managed variable
// (TOOL_PATH = "$PEKIT_ROOT/tools"), so parameter expansion has to survive.
// Emitting them bare — as this did originally — also let the shell word-split
// them, which made every multi-word value a syntax error (`export CFLAGS=-O2
// -pipe` → "export: -pipe: bad variable name"). Double quotes keep expansion
// and drop word-splitting, so both forms work.
//
// Backslash, double quote and backtick are escaped; `$` deliberately is not.
func shellExpandable(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '\\', '"', '`':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func appendQuotedExports(b *strings.Builder, values map[string]string) {
	keys := sortedKeys(values)
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(shellQuote(values[key]))
		b.WriteByte('\n')
	}
}
