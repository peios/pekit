package pekit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type sandboxCommand struct {
	Recipe    RecipeConfig
	Workspace WorkspaceConfig
	Source    SourceState
	Target    TargetConfig
	Profile   SandboxConfig
}

type buildJob struct {
	SourceInputs              *sourceInputs
	Secrets                   map[string]bool
	Stages                    map[string]bool
	Directory, Source, Recipe string
	Inputs                    map[string]string
}

func lockBuildJob(recipe RecipeConfig) (func(), error) {
	base := cleanSourceState(recipe).OutBase
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(recipe.Root, ".pekit-job.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, diag("job_busy", "another job owns %s", recipe.Root)
	}
	return func() { _ = f.Close() }, nil
}

// Sources have their own inodes. Symlinks stay symlinks and cannot make the
// copier read outside the input tree. In particular, never copy a Git database,
// source cache, keyring, or a configured private-key file into a worker.
func snapshotTree(src, dst, outBase string, secrets map[string]bool) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	src = resolved
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		excluded := sourceInputExcluded(path, outBase, secrets)
		if rel != "." && excluded {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(dest, info.Mode().Perm()|0700)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, dest)
		case info.Mode().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()|0600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			closeErr := out.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			return os.Chtimes(dest, info.ModTime(), info.ModTime())
		default:
			return fmt.Errorf("source input %s is not a regular file, directory or symlink", path)
		}
	})
}

func (s *sandboxCommand) job(ctx *Context) (*buildJob, error) {
	if ctx.Jobs == nil {
		ctx.Jobs = map[string]*buildJob{}
	}
	if job := ctx.Jobs[s.Source.WorkBase]; job != nil {
		return job, nil
	}
	sum := sha256.Sum256([]byte(s.Source.WorkBase))
	dir := filepath.Join(s.Source.OutBase, ".pekit-jobs", fmt.Sprintf("%x", sum[:12]))
	// Retain the private source with retained build outputs. A normal build gets
	// a fresh snapshot; --no-build explicitly asks to continue the previous job.
	safeStages := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(dir, "stages.json")); err == nil {
		if err := json.Unmarshal(data, &safeStages); err != nil {
			return nil, fmt.Errorf("invalid isolated stage record: %w", err)
		}
	}
	reuse := ctx.Inv.NoBuild != nil && dirExists(filepath.Join(dir, "source"))
	if !reuse {
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	job := &buildJob{Directory: dir, Source: filepath.Join(dir, "source"), Recipe: filepath.Join(dir, "recipe"), Inputs: map[string]string{}, Stages: safeStages}
	secrets := map[string]bool{}
	rings, err := resolveKeyrings(ctx.Inv, s.Recipe.Root, &s.Workspace)
	if err != nil {
		return nil, err
	}
	for _, value := range rings {
		if path, err := absPath(ctx.Inv.Cwd, value); err == nil {
			secrets[path] = true
			if real, err := filepath.EvalSymlinks(path); err == nil {
				secrets[real] = true
			}
		}
	}
	for _, value := range ctx.Inv.Keyrings {
		if path, err := resolveKeyringPath(value, ctx.Inv.Cwd, []string{s.Workspace.Root, s.Recipe.Root}); err == nil {
			secrets[path] = true
			if real, err := filepath.EvalSymlinks(path); err == nil {
				secrets[real] = true
			}
		}
	}
	secrets = sourceSnapshotExclusions(s.Workspace.Root, secrets)
	job.Secrets = secrets
	if !reuse {
		if err := snapshotTree(s.Source.SourceRoot, job.Source, s.Source.OutBase, secrets); err != nil {
			return nil, err
		}
	}
	if s.Source.SourceRoot == s.Recipe.Root {
		job.Recipe = job.Source
	} else if !reuse {
		if err := snapshotTree(s.Recipe.Root, job.Recipe, s.Source.OutBase, secrets); err != nil {
			return nil, err
		}
	}
	for i, rel := range append(append([]string{}, s.Workspace.Isolation.Inputs...), s.Recipe.SourcePackage.WorkspaceInputs...) {
		if err := validateSourceInputPath(s.Workspace.Root, rel); err != nil {
			return nil, err
		}
		input := filepath.Join(s.Workspace.Root, rel)
		resolved, err := filepath.EvalSymlinks(input)
		if err != nil {
			return nil, err
		}
		if !withinDirectory(s.Workspace.Root, resolved) || resolved == s.Workspace.Root {
			return nil, fmt.Errorf("workspace input %s escapes its workspace", rel)
		}
		copy := filepath.Join(dir, fmt.Sprintf("input-%d", i))
		if !reuse {
			if err := snapshotTree(resolved, copy, s.Source.OutBase, secrets); err != nil {
				return nil, err
			}
		}
		job.Inputs[input] = copy
	}
	// Recipe inputs are declared upstreams, already fetched and verified. Bind
	// each read-only at its own path: a worker must read the exact materialised
	// bytes and cannot alter them.
	for _, input := range s.Source.Inputs {
		if input.Root == "" {
			continue
		}
		job.Inputs[input.Root] = input.Root
	}
	controlRecord := filepath.Join(dir, "source-inputs.json")
	if reuse {
		data, err := os.ReadFile(controlRecord)
		if err != nil {
			return nil, diag("source_inputs_missing", "retained job predates captured source inputs; rebuild before reuse")
		}
		if err := json.Unmarshal(data, &job.SourceInputs); err != nil {
			return nil, err
		}
		if job.SourceInputs == nil {
			return nil, diag("source_inputs_missing", "retained job has no captured source inputs")
		}
		if err := job.SourceInputs.unchanged(); err != nil {
			return nil, err
		}
	} else {
		job.SourceInputs, err = captureSourceInputs(ctx, s.Recipe, &s.Workspace, s.Source, filepath.Join(dir, "source-inputs"), secrets, job.Inputs)
		if err != nil {
			return nil, err
		}
		if err := saveSourceInputs(controlRecord, job.SourceInputs); err != nil {
			return nil, err
		}
	}
	ctx.Jobs[s.Source.WorkBase] = job
	return job, nil
}

func withinDirectory(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Bind each stage directory itself, not its parent. A worker may modify other
// stages in its own job, but cannot replace their mountpoints with symlinks or
// touch completion records, cached inputs, published packages, or another job.
func sandboxStages(s *sandboxCommand, job *buildJob) ([]string, error) {
	var stages []string
	for _, kind := range []Command{CommandBuild, CommandTest, CommandInstall, CommandClean} {
		for _, name := range sortedKeys(s.Recipe.Targets[kind]) {
			stage := targetStage(s.Source, kind, name)
			if !dirExists(stage) || (stage != targetStage(s.Source, s.Target.Kind, s.Target.Name) && !job.Stages[stage]) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(stage)
			if err != nil {
				return nil, err
			}
			if resolved != stage {
				return nil, fmt.Errorf("stage path contains a symlink: %s", stage)
			}
			stages = append(stages, stage)
		}
	}
	return stages, nil
}

func executeSandbox(ctx *Context, command ShellCommand, env CommandEnv, cwd, member, version, target string) error {
	s := env.Sandbox
	job, err := s.job(ctx)
	if err != nil {
		return err
	}
	var before, baseline map[string]generatedEntry
	if s.Target.Kind == CommandGen {
		before, err = generatedInventory(job.Source, s.Source.SourceRoot, s.Source.OutBase, job.Secrets)
		if err != nil {
			return err
		}
		baseline, err = generatedInventory(s.Source.SourceRoot, s.Source.SourceRoot, s.Source.OutBase, job.Secrets)
		if err != nil {
			return err
		}
	}
	work, err := os.MkdirTemp("", "pekit-root-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, "root")
	outer := map[string]string{}
	overlay(outer, env.Outer)
	outer["PEKIT_SANDBOX_ROOT"] = root
	outer["PEKIT_JOB_STATE"] = job.Directory
	prep := CommandEnv{Outer: outer, Values: outer, Script: exportScript(outer)}
	if err := executeCommand(ctx, s.Profile.Prepare, prep, s.Workspace.Root, member, version, target+":root"); err != nil {
		return err
	}
	if !fileExists(filepath.Join(root, "usr/bin/sh")) && !fileExists(filepath.Join(root, "bin/sh")) {
		return fmt.Errorf("sandbox root preparer did not provide a shell")
	}
	if err := ensureSandboxLocalhost(root); err != nil {
		return err
	}
	if err := checkSandboxEntry(root, s.Profile.Entry); err != nil {
		return err
	}
	stages, err := sandboxStages(s, job)
	if err != nil {
		return err
	}
	for _, path := range append(append([]string{"/dev", "/proc", "/tmp", "/run", s.Recipe.Root, s.Source.SourceRoot}, stages...), env.Outer["PEKIT_OUT"]) {
		if err := makeMountpoint(root, path); err != nil {
			return err
		}
		for _, pair := range [][2]string{{s.Recipe.Root, job.Recipe}, {s.Source.SourceRoot, job.Source}} {
			if path != pair[0] && withinDirectory(pair[0], path) {
				rel, _ := filepath.Rel(pair[0], path)
				if err := makeMountpoint(pair[1], rel); err != nil {
					return err
				}
			}
		}
	}
	for _, path := range sortedKeys(job.Inputs) {
		if err := makeInputMountpoint(root, path, job.Inputs[path]); err != nil {
			return err
		}
	}
	args := []string{"--die-with-parent", "--unshare-all", "--unshare-user", "--new-session", "--disable-userns", "--uid", "1000", "--gid", "1000", "--ro-bind", root, "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run"}
	network := false
	for _, name := range s.Profile.NetworkTargets {
		if name == target {
			network = true
		}
	}
	if network {
		config, err := os.ReadFile("/etc/resolv.conf")
		if err != nil {
			return err
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return err
		}
		if err := r.MkdirAll("etc", 0755); err != nil {
			r.Close()
			return err
		}
		// Replace a possible image symlink without following it outside root.
		_ = r.Remove("etc/resolv.conf")
		f, err := r.OpenFile("etc/resolv.conf", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			r.Close()
			return err
		}
		_, err = f.Write(config)
		closeErr := f.Close()
		r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		args = append(args, "--share-net")
	}
	args = append(args, "--bind", job.Recipe, s.Recipe.Root)
	if s.Source.SourceRoot != s.Recipe.Root {
		args = append(args, "--bind", job.Source, s.Source.SourceRoot)
	}
	for _, path := range sortedKeys(job.Inputs) {
		args = append(args, "--ro-bind", job.Inputs[path], path)
	}
	for _, stage := range stages {
		args = append(args, "--bind", stage, stage)
	}
	if s.Target.Kind == CommandVerify || s.Target.Kind == CommandGen {
		args = append(args, "--bind", env.Outer["PEKIT_OUT"], env.Outer["PEKIT_OUT"])
	}
	args = append(args, "--ro-bind", env.Outer["PEKIT_DEPENDENCIES_FILE"], "/run/pekit-dependencies.json")
	args = append(args, "--chdir", cwd, "--clearenv", "--setenv", "PATH", "/usr/libexec/coreutils-build:/usr/bin:/bin:/usr/sbin:/sbin", "--setenv", "HOME", "/tmp", "--setenv", "USER", "peibuild", "--setenv", "LOGNAME", "peibuild", "--setenv", "CARGO_HOME", "/tmp/cargo")
	if env.Outer["PEKIT_DEPENDENCY_PROVIDER"] == "peipkg" && !network {
		args = append(args, "--setenv", "PEKIT_NATIVE_ROOT", "1")
	}
	script := command.Shell
	if script == "" {
		script = "exec " + shellJoin(command.Args)
	}
	// A profile entry runs first, inside the sandbox, and execs the target.
	args = append(args, s.Profile.Entry...)
	args = append(args, "/bin/sh", "-euc", env.Script+"\n"+script)
	// bwrap owns PID 1 in the new namespace. When the target exits it kills and
	// reaps remaining descendants before returning to coordinator signing.
	if err := runProcess(ctx, s.Workspace.Root, member, version, target, "bwrap", args, map[string]string{}); err != nil {
		return err
	}
	if s.Target.Kind == CommandGen {
		return syncGeneratedSource(s, job, before, baseline)
	}
	return nil
}

// checkSandboxEntry requires the preparer to have supplied the profile's entry
// program as an executable regular file, resolved within the root.
func checkSandboxEntry(root string, entry []string) error {
	if len(entry) == 0 {
		return nil
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := r.Stat(strings.TrimPrefix(entry[0], "/"))
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return diag("sandbox_entry", "sandbox root preparer did not provide executable entry %s", entry[0])
	}
	return nil
}

func makeMountpoint(root, path string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.MkdirAll(strings.TrimPrefix(path, "/"), 0755)
}

func (j *buildJob) recordStage(stage string) error {
	j.Stages[stage] = true
	data, err := json.Marshal(j.Stages)
	if err != nil {
		return err
	}
	temp := filepath.Join(j.Directory, "stages.json.tmp")
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(j.Directory, "stages.json"))
}

func makeInputMountpoint(root, path, source string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return makeMountpoint(root, path)
	}
	if err := makeMountpoint(root, filepath.Dir(path)); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := r.OpenFile(strings.TrimPrefix(path, "/"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Docker exports omit its bind-mounted hosts file. Supply deterministic local
// resolution when a preparer leaves it empty, without exposing host aliases.
func ensureSandboxLocalhost(root string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.MkdirAll("etc", 0755); err != nil {
		return err
	}
	info, err := r.Lstat("etc/hosts")
	if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("sandbox hosts path is not a file")
		}
		if err := r.Remove("etc/hosts"); err != nil {
			return err
		}
	}
	f, err := r.OpenFile("etc/hosts", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = f.WriteString("127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n")
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
