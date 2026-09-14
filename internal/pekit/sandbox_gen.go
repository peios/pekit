package pekit

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type generatedEntry struct {
	Mode fs.FileMode
	Hash [32]byte
	Link string
}

func generatedInventory(root, logicalRoot, outBase string, secrets map[string]bool) (map[string]generatedEntry, error) {
	entries := map[string]generatedEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := d.Name()
		logical := filepath.Join(logicalRoot, rel)
		if logical == outBase || secrets[logical] || name == ".git" || name == ".env" || name == ".pekit-job.lock" || name == "credentials" || name == "credentials.toml" || strings.HasSuffix(name, ".keyring.pekit.toml") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		entry := generatedEntry{Mode: st.Mode()}
		switch {
		case st.IsDir():
		case st.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Hash = sha256.Sum256(data)
		case st.Mode()&os.ModeSymlink != 0:
			entry.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("generator produced unsupported file type at %s", rel)
		}
		entries[rel] = entry
		return nil
	})
	return entries, err
}

// Only explicit `gen` writes back. Verify/build/test leave the checkout alone.
// Detect concurrent source edits before the first write and use os.Root for
// every write, so generated symlinks cannot escape the authorized source tree.
func syncGeneratedSource(s *sandboxCommand, job *buildJob, before, baseline map[string]generatedEntry) error {
	after, err := generatedInventory(job.Source, s.Source.SourceRoot, s.Source.OutBase, job.Secrets)
	if err != nil {
		return err
	}
	current, err := generatedInventory(s.Source.SourceRoot, s.Source.SourceRoot, s.Source.OutBase, job.Secrets)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(baseline, current) {
		return fmt.Errorf("source changed during generation; refusing to overwrite concurrent edits")
	}
	changed := map[string]bool{}
	for path, entry := range before {
		if other, ok := after[path]; !ok || other != entry {
			changed[path] = true
		}
	}
	for path, entry := range after {
		if other, ok := before[path]; !ok || other != entry {
			changed[path] = true
		}
	}
	for path := range changed {
		original, existed := baseline[path]
		now, exists := current[path]
		if existed != exists || original != now {
			return fmt.Errorf("source changed during generation: %s", path)
		}
	}
	root, err := os.OpenRoot(s.Source.SourceRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	paths := sortedKeys(changed)
	// Delete leaves before directories. Remove (never RemoveAll) protects
	// excluded local files in a directory the generator attempted to remove.
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		old, exists := before[path]
		if !exists {
			continue
		}
		next, kept := after[path]
		if kept && old.Mode.Type() == next.Mode.Type() && !old.Mode.IsDir() {
			continue
		}
		if kept && old.Mode.IsDir() && next.Mode.IsDir() {
			continue
		}
		if err := root.Remove(path); err != nil {
			return err
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		if len(paths[i]) != len(paths[j]) {
			return len(paths[i]) < len(paths[j])
		}
		return paths[i] < paths[j]
	})
	for _, path := range paths {
		entry, exists := after[path]
		if !exists {
			continue
		}
		if entry.Mode.IsDir() {
			if err := root.MkdirAll(path, entry.Mode.Perm()); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if entry.Mode&os.ModeSymlink != 0 {
			if _, ok := before[path]; ok {
				_ = root.Remove(path)
			}
			if err := root.Symlink(entry.Link, path); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(job.Source, path))
		if err != nil {
			return err
		}
		// The temp name cannot be chosen by the generator, and is created inside
		// the source root. Rename replaces a link instead of following its target.
		temp := path + ".pekit-gen-tmp"
		f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.Mode.Perm())
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			_ = root.Remove(temp)
			return err
		}
		if closeErr != nil {
			_ = root.Remove(temp)
			return closeErr
		}
		if err := root.Rename(temp, path); err != nil {
			_ = root.Remove(temp)
			return err
		}
	}
	return nil
}
