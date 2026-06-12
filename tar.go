package main

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// tarEngine packages a build stage into <name>.tar in the package stage.
// Output is deterministic: entries in lexical order (WalkDir guarantees
// it), owner zeroed, every timestamp at the epoch — identical inputs give
// byte-identical archives, which signing and caching depend on.
func tarEngine(job PackageJob) error {
	outPath := filepath.Join(job.OutStage, job.Name+".tar")
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	walkErr := filepath.WalkDir(job.BuildStage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == job.BuildStage {
			return nil
		}
		rel, err := filepath.Rel(job.BuildStage, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		hdr.ModTime = time.Unix(0, 0)
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("package %s: %w", job.Name, walkErr)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	fmt.Printf("pekit: wrote %s\n", outPath)
	return nil
}
