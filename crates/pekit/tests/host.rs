//! Tests of the host snapshot and its fingerprint.

use std::fs;
use std::path::Path;

use pekit::host::HostSnapshot;

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

#[test]
fn a_snapshot_changes_with_the_host_s_packages_and_alternatives() {
    let root = tempfile::tempdir().unwrap();
    let root = root.path();
    // No package database pekit knows: no fingerprint.
    write(root, &[("etc/os-release", "ID=test\n"), ("usr/bin/sh", "")]);
    let none = HostSnapshot::of_root(root);
    assert_eq!(none.fingerprint, None);
    assert_eq!(none.dirs, vec![Path::new("/usr").to_path_buf()]);
    assert!(
        none.platform("x86_64-linux")
            .ends_with("host-snapshot:unknown")
    );
    // dpkg's: one, the same while nothing changes.
    write(
        root,
        &[("var/lib/dpkg/status", "Package: gcc\nVersion: 15\n")],
    );
    let first = HostSnapshot::of_root(root).fingerprint.unwrap();
    assert_eq!(HostSnapshot::of_root(root).fingerprint, Some(first));
    // A package upgraded: another.
    write(
        root,
        &[("var/lib/dpkg/status", "Package: gcc\nVersion: 16\n")],
    );
    let upgraded = HostSnapshot::of_root(root).fingerprint.unwrap();
    assert_ne!(first, upgraded);
    // An alternative chosen, with no package changed: another.
    fs::create_dir_all(root.join("etc/alternatives")).unwrap();
    std::os::unix::fs::symlink("/usr/bin/gcc-16", root.join("etc/alternatives/cc")).unwrap();
    let chosen = HostSnapshot::of_root(root).fingerprint.unwrap();
    assert_ne!(chosen, upgraded);
    fs::remove_file(root.join("etc/alternatives/cc")).unwrap();
    std::os::unix::fs::symlink("/usr/bin/clang", root.join("etc/alternatives/cc")).unwrap();
    assert_ne!(HostSnapshot::of_root(root).fingerprint.unwrap(), chosen);
    // The platform names it.
    let snapshot = HostSnapshot::of_root(root);
    assert!(
        snapshot
            .platform("x86_64-linux")
            .starts_with("x86_64-linux host-snapshot:")
    );
}
