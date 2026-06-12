package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// tarEngine packages the resolved [files] mapping into <name>.tar in the
// package stage. Output is deterministic: entries in lexical order with
// implied parent directories synthesized, owner zeroed, every timestamp
// at the epoch — identical inputs give byte-identical archives, which
// signing and caching depend on.
func tarEngine(job PackageJob) error {
	outPath := filepath.Join(job.OutStage, job.Name+".tar")
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	for _, dir := range impliedDirs(job.Files) {
		hdr := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     dir + "/",
			Mode:     0o755,
			ModTime:  time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("package %s: %w", job.Name, err)
		}
	}
	for _, sf := range job.Files {
		if err := writeTarFile(tw, sf); err != nil {
			return fmt.Errorf("package %s: %w", job.Name, err)
		}
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

func writeTarFile(tw *tar.Writer, sf StagedFile) error {
	info, err := os.Stat(sf.Source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source %s is not a regular file", sf.Source)
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = sf.Dest
	hdr.Uid, hdr.Gid = 0, 0
	hdr.Uname, hdr.Gname = "", ""
	hdr.ModTime = time.Unix(0, 0)
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	file, err := os.Open(sf.Source)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(tw, file)
	return err
}

// impliedDirs returns every parent directory implied by the dests,
// sorted, so archives carry their tree structure explicitly instead of
// relying on extractor defaults.
func impliedDirs(files []StagedFile) []string {
	set := make(map[string]bool)
	for _, sf := range files {
		for dir := path.Dir(sf.Dest); dir != "." && dir != "/"; dir = path.Dir(dir) {
			set[dir] = true
		}
	}
	dirs := make([]string, 0, len(set))
	for dir := range set {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}
