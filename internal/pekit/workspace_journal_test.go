package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// journalWorkspace makes a workspace of sourceless members whose builds append
// a line to <dir>/<name>.runs, so a test can count how often each ran.
// commands overrides a member's build command.
func journalWorkspace(t *testing.T, names []string, commands map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	for _, name := range names {
		command := commands[name]
		if command == "" {
			command = "echo run >> '" + filepath.Join(dir, name+".runs") + "'"
		}
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), "out_dir = \"out\"\n\n[build]\ncommand = '''"+command+"'''\n")
	}
	chdir(t, dir)
	return dir
}

func runCount(t *testing.T, dir, name string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "run\n")
}

func runWorkspaceArgs(args ...string) error {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	return app.Run(args)
}

// A member that finished is skipped when the same run is repeated; one that
// failed is not recorded and runs again.
func TestJournalSkipsFinishedMembersAndRetriesFailures(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "b-may-pass")
	ws := journalWorkspace(t, []string{"a", "b"}, map[string]string{
		"b": "test -e '" + flag + "' && echo run >> '" + filepath.Join(dir, "b.runs") + "'",
	})
	journal := filepath.Join(ws, "round.journal")
	if err := runWorkspaceArgs("workspace", "--journal", journal, "build"); diagCode(err) != "workspace_failed" {
		t.Fatalf("first run = %v, want workspace_failed", err)
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); len(got) != 2 || got[1] != "a" {
		t.Fatalf("journal = %q, want the run line and a", data)
	}
	writeFile(t, flag, "")
	if err := runWorkspaceArgs("workspace", "--journal", journal, "build"); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if a, b := runCount(t, ws, "a"), runCount(t, dir, "b"); a != 1 || b != 1 {
		t.Fatalf("runs: a=%d b=%d, want a once (not repeated) and b once", a, b)
	}
}

// Creating the stop file lets running members finish, starts nothing new, and
// exits workspace_stopped; the stop file is consumed and a repeat resumes.
func TestJournalStopFileStopsBetweenMembers(t *testing.T) {
	var journal string
	ws := journalWorkspace(t, []string{"a", "b"}, nil)
	journal = filepath.Join(ws, "round.journal")
	// a asks for the stop while it runs.
	writeFile(t, filepath.Join(ws, "a", "pekit.toml"), "out_dir = \"out\"\n\n[build]\ncommand = '''echo run >> '"+filepath.Join(ws, "a.runs")+"' && touch '"+journal+".stop''''\n")
	err := runWorkspaceArgs("workspace", "--jobs", "1", "--journal", journal, "build")
	if diagCode(err) != "workspace_stopped" {
		t.Fatalf("run = %v, want workspace_stopped", err)
	}
	if a, b := runCount(t, ws, "a"), runCount(t, ws, "b"); a != 1 || b != 0 {
		t.Fatalf("runs: a=%d b=%d, want a finished and b not started", a, b)
	}
	if fileExists(journal + ".stop") {
		t.Fatal("the stop request was not consumed")
	}
	// Resuming runs only b (a would request another stop if it ran again).
	if err := runWorkspaceArgs("workspace", "--journal", journal, "build"); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if a, b := runCount(t, ws, "a"), runCount(t, ws, "b"); a != 1 || b != 1 {
		t.Fatalf("runs after resume: a=%d b=%d, want 1 and 1", a, b)
	}
}

// A journal belongs to one run: a different command or selection is refused
// rather than skipping members that never ran under it. --jobs may change.
func TestJournalRefusesADifferentRun(t *testing.T) {
	ws := journalWorkspace(t, []string{"a"}, nil)
	journal := filepath.Join(ws, "round.journal")
	if err := runWorkspaceArgs("workspace", "--jobs", "2", "--journal", journal, "build"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := runWorkspaceArgs("workspace", "--jobs", "1", "--journal", journal, "build"); err != nil {
		t.Fatalf("same run with different --jobs: %v", err)
	}
	if err := runWorkspaceArgs("workspace", "--journal", journal, "build", "--latest"); diagCode(err) != "journal_mismatch" {
		t.Fatalf("different selection = %v, want journal_mismatch", err)
	}
	if got := runCount(t, ws, "a"); got != 1 {
		t.Fatalf("a ran %d times, want once", got)
	}
}

// A dry run reads the journal but neither creates nor changes it.
func TestJournalDryRunWritesNothing(t *testing.T) {
	ws := journalWorkspace(t, []string{"a"}, nil)
	journal := filepath.Join(ws, "round.journal")
	if err := runWorkspaceArgs("--dry-run", "workspace", "--journal", journal, "build"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if fileExists(journal) {
		t.Fatal("a dry run created the journal")
	}
}

// A member the journal records as finished still carries its packages'
// runtime dependencies into the order: a-app installs test.lib, defined by the
// finished z-lib, which needs test.tool, so a-app still waits for m-tool.
func TestJournalFinishedMembersStillOrderTheirDependents(t *testing.T) {
	dir, log := orderingWorkspace(t)
	journal := filepath.Join(dir, "round.journal")
	writeFile(t, journal, journalRunPrefix+`build env="peipkg"`+"\nz-lib\n")
	out, err := runWorkspaceIn(t, dir, "workspace", "--jobs", "4", "--journal", journal, "build", "--env", "peipkg")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	at := func(line string) int {
		for i, l := range lines {
			if l == line {
				return i
			}
		}
		return -1
	}
	if at("start z-lib") != -1 {
		t.Fatalf("the finished z-lib ran again:\n%s", data)
	}
	if at("start a-app") < at("end m-tool") {
		t.Fatalf("a-app started before m-tool, which the finished z-lib's package needs:\n%s", data)
	}
}
