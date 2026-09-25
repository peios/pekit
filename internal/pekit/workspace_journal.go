package pekit

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// A workspace journal makes a long workspace run resumable. Every member that
// finishes successfully is appended to the journal, and a later run of the
// same command with the same journal skips those members. A run is stopped
// cleanly by creating the journal's stop file (the journal path plus
// ".stop"): members already running finish, nothing new starts, and the run
// exits with workspace_stopped, ready to resume.
//
// The journal's first line records the run it belongs to, so resuming with a
// different command, selection or environment is refused rather than
// silently skipping members that never ran under it. --jobs and --fail-fast
// are not part of that identity: changing them on resume is expected.
type workspaceJournal struct {
	path string
	done map[string]bool
	file *os.File
}

const journalRunPrefix = "# run: "

func journalRunIdentity(inv Invocation) string {
	parts := []string{string(inv.DelegateCommand)}
	add := func(name, value string) {
		if value != "" {
			parts = append(parts, name+"="+strconv.Quote(value))
		}
	}
	flag := func(name string, set bool) {
		if set {
			parts = append(parts, name)
		}
	}
	add("version", inv.Version)
	flag("latest", inv.Latest)
	flag("all-versions", inv.AllVersions)
	flag("locked", inv.Locked)
	flag("all", inv.All)
	flag("replace", inv.Replace)
	flag("strict", inv.Strict)
	flag("no-gates", inv.NoGates)
	add("env", inv.EnvName)
	add("tags", strings.Join(inv.Tags, ","))
	add("exclude-tags", strings.Join(inv.ExcludeTags, ","))
	add("selectors", strings.Join(inv.Positionals, ","))
	return strings.Join(parts, " ")
}

func (j *workspaceJournal) stopPath() string { return j.path + ".stop" }

// openWorkspaceJournal loads the journal named by --journal, creating it for a
// real run. A dry run reads an existing journal but writes nothing.
func openWorkspaceJournal(ctx *Context) (*workspaceJournal, error) {
	if ctx.Inv.Journal == "" {
		return nil, nil
	}
	path, err := absPath(ctx.Inv.Cwd, ctx.Inv.Journal)
	if err != nil {
		return nil, wrapDiag("invalid_journal", "--journal", err)
	}
	j := &workspaceJournal{path: path, done: map[string]bool{}}
	identity := journalRunIdentity(ctx.Inv)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := j.load(string(data), identity); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrNotExist):
		if ctx.Inv.DryRun {
			return j, nil
		}
		if err := os.WriteFile(path, []byte(journalRunPrefix+identity+"\n"), 0o644); err != nil {
			return nil, wrapDiag("invalid_journal", "create journal", err)
		}
	default:
		return nil, wrapDiag("invalid_journal", "read journal", err)
	}
	if ctx.Inv.DryRun {
		return j, nil
	}
	// A stop file left from an earlier run asked that run to stop, not this one.
	if err := os.Remove(j.stopPath()); err == nil {
		ctx.Renderer.Event(Event{Type: "warning", Path: j.stopPath(), Message: "removed a stop file left by an earlier run"})
	}
	j.file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, wrapDiag("invalid_journal", "open journal", err)
	}
	return j, nil
}

func (j *workspaceJournal) load(data, identity string) error {
	scanner := bufio.NewScanner(strings.NewReader(data))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if first {
			first = false
			recorded, ok := strings.CutPrefix(line, journalRunPrefix)
			if !ok {
				return diag("invalid_journal", "%s is not a pekit workspace journal", j.path)
			}
			if recorded != identity {
				return diag("journal_mismatch", "%s belongs to a different run (%s); this run is %s. Use a new journal for it", j.path, recorded, identity)
			}
			continue
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			j.done[line] = true
		}
	}
	if first {
		return diag("invalid_journal", "%s is empty; delete it to start a new run", j.path)
	}
	return nil
}

// record appends a finished member and syncs, so a power loss straight
// afterwards still resumes past it.
func (j *workspaceJournal) record(id string) error {
	if j.file == nil {
		return nil
	}
	if _, err := fmt.Fprintln(j.file, id); err != nil {
		return err
	}
	return j.file.Sync()
}

// stopRequested reports, and consumes, a stop request.
func (j *workspaceJournal) stopRequested() bool {
	if j.file == nil {
		return false
	}
	return os.Remove(j.stopPath()) == nil
}

func (j *workspaceJournal) close() {
	if j != nil && j.file != nil {
		_ = j.file.Close()
	}
}
