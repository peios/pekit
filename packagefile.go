package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// PackageFile is a parsed package.pekit.toml: one file, one package.
// Strictly minimal — fields grow only when a format needs them.
type PackageFile struct {
	Format string
	Name   string        // optional override; defaults to the project dir name
	Files  []FileMapping // sorted by Dest
}

// FileMapping maps one build output to its install path.
type FileMapping struct {
	Source SourceRef
	Dest   string // image-relative, cleaned, no leading slash
}

// SourceRef is a [files] source. "target:path" references path inside
// build target's stage (":path" is sugar for "main:path"); a plain path
// is project-relative, for build systems that cannot redirect output
// into the stage.
type SourceRef struct {
	Target string // build target whose stage holds Path; empty = literal
	Path   string
}

func (s SourceRef) String() string {
	if s.Target == "" {
		return s.Path
	}
	return s.Target + ":" + s.Path
}

// LoadPackageFile reads and parses a package.pekit.toml.
func LoadPackageFile(path string) (*PackageFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pf, err := ParsePackageFile(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pf, nil
}

// ParsePackageFile parses package.pekit.toml source.
func ParsePackageFile(src string) (*PackageFile, error) {
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}

	pf := &PackageFile{}

	for _, key := range sortedKeys(raw) {
		switch key {
		case "format":
			s, err := stringValue("root", key, raw[key])
			if err != nil {
				return nil, err
			}
			pf.Format = s
		case "package":
			table, ok := raw[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[package] must be a table")
			}
			for _, pkey := range sortedKeys(table) {
				if pkey != "name" {
					return nil, fmt.Errorf("[package]: unknown key %q", pkey)
				}
				s, err := stringValue("package", pkey, table[pkey])
				if err != nil {
					return nil, err
				}
				pf.Name = s
			}
		case "files":
			table, ok := raw[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[files] must be a table")
			}
			files, err := parseFiles(table)
			if err != nil {
				return nil, err
			}
			pf.Files = files
		default:
			return nil, fmt.Errorf("unknown key %q", key)
		}
	}

	if pf.Format == "" {
		return nil, fmt.Errorf("missing required key %q", "format")
	}
	if len(pf.Files) == 0 {
		return nil, fmt.Errorf("[files] must map at least one file")
	}
	return pf, nil
}

func parseFiles(table map[string]any) ([]FileMapping, error) {
	var files []FileMapping
	seen := make(map[string]string) // dest -> source, for duplicate detection

	for _, key := range sortedKeys(table) {
		dest, ok := table[key].(string)
		if !ok {
			return nil, fmt.Errorf("[files]: %q must map to a destination string", key)
		}
		src, err := parseSourceRef(key)
		if err != nil {
			return nil, err
		}
		cleaned, err := cleanDest(dest)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[cleaned]; dup {
			return nil, fmt.Errorf("[files]: %q and %q both map to %q", prev, key, cleaned)
		}
		seen[cleaned] = key
		files = append(files, FileMapping{Source: src, Dest: cleaned})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Dest < files[j].Dest })
	return files, nil
}

func parseSourceRef(key string) (SourceRef, error) {
	if key == "" {
		return SourceRef{}, fmt.Errorf("[files]: sources must not be empty")
	}
	target, p, isRef := strings.Cut(key, ":")
	if !isRef {
		return SourceRef{Path: key}, nil
	}
	if target == "" {
		target = "main" // ":path" is sugar for "main:path"
	}
	if p == "" {
		return SourceRef{}, fmt.Errorf("[files]: source %q names no file inside target %s's stage", key, target)
	}
	cleaned := path.Clean(p)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return SourceRef{}, fmt.Errorf("[files]: source %q must stay inside the stage", key)
	}
	return SourceRef{Target: target, Path: cleaned}, nil
}

func cleanDest(dest string) (string, error) {
	if dest == "" {
		return "", fmt.Errorf("[files]: destinations must not be empty")
	}
	cleaned := path.Clean(dest)
	if path.IsAbs(cleaned) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("[files]: destination %q must be a relative path inside the package", dest)
	}
	return cleaned, nil
}
