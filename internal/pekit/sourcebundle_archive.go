package pekit

import (
	"io"
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
				if !sourceLinkContained(sourceRoot, entry.Source) {
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
	archive := filepath.Join(stage, "prepared-source.tar.gz")
	if err := writeTarArchive(archive, packed, true); err != nil {
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
	manifest.Schema = 4
	manifest.PreparedArchive = "prepared-source.tar.gz"
	manifest.Files = append(manifest.Files, sourceBundleFile{Path: manifest.PreparedArchive, Identity: identity})
	return append(outer, payloadEntry{Source: archive, Dest: root + "/" + manifest.PreparedArchive}), nil
}

// Limit the decoded tar stream as well as its compressed file size. The generic
// consumer enforces the same cap, including metadata/header/padding bytes.
type preparedTarWriter struct {
	writer  io.Writer
	written int64
}

func (w *preparedTarWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > (4<<30)-w.written {
		return 0, diag("source_bundle_limit", "prepared source tar stream exceeds 4 GiB")
	}
	n, err := w.writer.Write(p)
	w.written += int64(n)
	return n, err
}
