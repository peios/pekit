package pekit

import (
	"archive/tar"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/peios/peipkg/repopub"
)

// Each payload object is materialized at an unrelated flat path. No archive
// symlink can redirect extraction, and lint still sees its original link target.
func lintReleaseArchives(l *linter, instances []PackageInstance, key ed25519.PrivateKey, dir, workBase string, version Version) error {
	set := &lintPayloadSet{byName: map[string]*lintPackage{}, union: map[string]string{}, claims: map[string]bool{}, workBase: workBase, elfCache: map[string]*elfInfo{}}
	scratch, err := os.MkdirTemp(dir, "lint-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	for i, inst := range instances {
		info, err := repopub.InspectPackage(inst.Artifact, []ed25519.PublicKey{key.Public().(ed25519.PublicKey)})
		if err != nil {
			return err
		}
		if !info.Signed {
			return diag("unsigned_release", "unsigned candidate archive")
		}
		var metadata struct {
			Name, Version, Architecture string
			Dependencies                []struct{ Name, Constraint string }
		}
		if err = json.Unmarshal(info.ManifestJSON, &metadata); err != nil {
			return err
		}
		if metadata.Name != inst.Name || metadata.Version != inst.Version || metadata.Architecture != inst.Architecture {
			return diag("release_identity", "candidate archive identity differs from selection")
		}
		// Source archives still undergo full archive/signature verification. Binary
		// payload rules (ELF placement, executable paths etc.) do not apply to them.
		if inst.SourcePkg {
			continue
		}
		inst.Config.Package.Dependencies = map[string]string{}
		for _, d := range metadata.Dependencies {
			inst.Config.Package.Dependencies[d.Name] = d.Constraint
		}
		pkg := lintPackage{Inst: inst, Files: map[string]payloadEntry{}}
		for j, p := range info.Payload {
			flat := filepath.Join(scratch, fmt.Sprintf("%d-%d", i, j))
			if p.Directory {
				err = os.Mkdir(flat, 0755)
			} else if p.Symlink {
				err = os.Symlink(p.LinkTarget, flat)
			}
			if err != nil {
				return err
			}
			pkg.Files[p.Path] = payloadEntry{Source: flat, Dest: p.Path}
			set.union[p.Path] = inst.Name
		}
		f, err := os.Open(inst.Artifact)
		if err != nil {
			return err
		}
		z, err := zstd.NewReader(f)
		if err != nil {
			f.Close()
			return err
		}
		tr := tar.NewReader(z)
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				z.Close()
				f.Close()
				return e
			}
			entry, ok := pkg.Files[h.Name]
			if !ok {
				continue
			}
			if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
				out, e := os.OpenFile(entry.Source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
				if e != nil {
					z.Close()
					f.Close()
					return e
				}
				_, e = io.Copy(out, tr)
				ce := out.Close()
				if e != nil {
					z.Close()
					f.Close()
					return e
				}
				if ce != nil {
					z.Close()
					f.Close()
					return ce
				}
			}
		}
		z.Close()
		f.Close()
		for dest, entry := range pkg.Files {
			if _, e := os.Lstat(entry.Source); e != nil {
				return fmt.Errorf("candidate payload missing during lint: %s: %w", dest, e)
			}
		}
		pkg.Dests = sortedKeys(pkg.Files)
		for _, side := range []map[string]map[string]ClaimSlot{inst.Config.Package.Claims.Provides, inst.Config.Package.Claims.Dependencies} {
			for _, slots := range side {
				for _, slot := range slots {
					if slot.Path != "" {
						p, e := cleanRelPath(slot.Path)
						if e != nil {
							return e
						}
						set.claims[p] = true
					}
				}
			}
		}
		set.Packages = append(set.Packages, pkg)
	}
	for i := range set.Packages {
		set.byName[set.Packages[i].Inst.Name] = &set.Packages[i]
	}
	for _, pkg := range set.Packages {
		lintPackagePayload(l, set, pkg, version)
	}
	return nil
}
