package main

import (
	"os"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

// builtLedger is the durable record of versions already built for a
// recipe (pekit.built, one version per line). At farm scale the build
// artifacts are deleted after publishing to reclaim disk, so this file
// is the only memory of what's done — it lets an enumerate run skip work
// it has already completed.
type builtLedger struct {
	path string
	set  map[string]bool
}

// loadLedger reads pekit.built; a missing file is an empty ledger.
func loadLedger(path string) (*builtLedger, error) {
	l := &builtLedger{path: path, set: map[string]bool{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			l.set[line] = true
		}
	}
	return l, nil
}

func (l *builtLedger) has(version string) bool {
	return l.set[version]
}

// add records a version and rewrites the file atomically (temp+rename),
// so a crash mid-run never leaves a corrupt or partial ledger.
func (l *builtLedger) add(version string) error {
	if l.set[version] {
		return nil
	}
	l.set[version] = true

	versions := make([]string, 0, len(l.set))
	for v := range l.set {
		versions = append(versions, v)
	}
	sortVersionStrings(versions)

	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(versions, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// sortVersionStrings sorts by semver where possible (so 0.9.0 precedes
// 0.10.0 for clean diffs), falling back to lexical for non-semver lines.
func sortVersionStrings(versions []string) {
	sort.Slice(versions, func(i, j int) bool {
		a, errA := semver.NewVersion(versions[i])
		b, errB := semver.NewVersion(versions[j])
		if errA != nil || errB != nil {
			return versions[i] < versions[j]
		}
		return a.LessThan(b)
	})
}
