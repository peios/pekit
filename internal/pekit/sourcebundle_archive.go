package pekit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Keep the outer package within Peipkg's 100,000-entry limit. The margin covers
// generated metadata and parent directories. Identity records remain per file.
func compactSourceBundle(entries []payloadEntry, manifest *sourceBundleManifest, root, stage string, threshold int) ([]payloadEntry, error) {
	if len(entries) < threshold {
		return entries, nil
	}
	var packed, outer []payloadEntry
	prefix := root + "/source"
	for _, entry := range entries {
		if entry.Dest == prefix || strings.HasPrefix(entry.Dest, prefix+"/") {
			if info, err := os.Lstat(entry.Source); err != nil {
				return nil, err
			} else if info.Mode()&os.ModeSymlink != 0 {
				sourceRoot := entry.Source
				for range strings.Split(strings.TrimPrefix(entry.Dest, prefix+"/"), "/") {
					sourceRoot = filepath.Dir(sourceRoot)
				}
				real, err := filepath.EvalSymlinks(entry.Source)
				if err != nil || !withinDirectory(sourceRoot, real) {
					return nil, diag("source_input_escape", "packed source link escapes prepared tree: %s", entry.Dest)
				}
			}
			entry.Dest = strings.TrimPrefix(entry.Dest, root+"/")
			packed = append(packed, entry)
		} else {
			outer = append(outer, entry)
		}
	}
	if len(packed) == 0 || len(outer)+4 >= threshold {
		return nil, diag("source_bundle_limit", "too many non-source bundle entries")
	}
	sort.Slice(packed, func(i, j int) bool { return packed[i].Dest < packed[j].Dest })
	archive := filepath.Join(stage, "prepared-source.tar")
	if err := writeTar(archive, packed); err != nil {
		return nil, err
	}
	info, err := os.Stat(archive)
	if err != nil {
		return nil, err
	}
	if info.Size() > 4<<30 {
		return nil, diag("source_bundle_limit", "prepared source archive exceeds 4 GiB")
	}
	identity, err := inputIdentity(archive)
	if err != nil {
		return nil, err
	}
	manifest.Schema = 3
	manifest.PreparedArchive = "prepared-source.tar"
	manifest.Files = append(manifest.Files, sourceBundleFile{Path: manifest.PreparedArchive, Identity: identity})
	return append(outer, payloadEntry{Source: archive, Dest: root + "/" + manifest.PreparedArchive}), nil
}
