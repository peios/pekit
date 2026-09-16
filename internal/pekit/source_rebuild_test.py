"""Adversarial reconstruction tests invoked by Go against the embedded script."""
import hashlib
import io
import json
import os
import pathlib
import subprocess
import sys
import tarfile
import tempfile
import unittest

SCRIPT = pathlib.Path(sys.argv.pop()).read_bytes()


class RebuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = pathlib.Path(self.tmp.name)
        (self.root / "rebuild.py").write_bytes(SCRIPT)
        (self.root / "bin").mkdir()
        stub = self.root / "bin/pekit"
        stub.write_text("#!/bin/sh\nprintf invoked > invoked\n")
        stub.chmod(0o755)
        self.files = {"source": "dir:0755", "source/file": "0640:" + hashlib.sha256(b"hello").hexdigest(), "source/link": "link:file"}
        self.members = [("source/", tarfile.DIRTYPE, b"", ""), ("source/file", tarfile.REGTYPE, b"hello", ""), ("source/link", tarfile.SYMTYPE, b"", "file")]

    def bundle(self):
        archive = self.root / "prepared-source.tar"
        with tarfile.open(archive, "w") as tar:
            for name, kind, content, link in self.members:
                member = tarfile.TarInfo(name)
                member.type, member.size, member.linkname = kind, len(content), link
                tar.addfile(member, io.BytesIO(content))
        self.files[archive.name] = "0644:" + hashlib.sha256(archive.read_bytes()).hexdigest()
        self.manifest = {"schema": 3, "prepared_archive": archive.name, "recipe": "recipe", "files": [{"path": p, "identity": i} for p, i in self.files.items()]}
        (self.root / "recipe").mkdir(exist_ok=True)
        self.save()

    def save(self):
        (self.root / "build-inputs.json").write_text(json.dumps(self.manifest))

    def run_bundle(self, success=False):
        result = subprocess.run(["python3", "rebuild.py", "build"], cwd=self.root, env={**os.environ, "PATH": str(self.root / "bin") + ":" + os.environ["PATH"]}, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stderr)
        self.assertEqual((self.root / "invoked").exists(), success, result.stderr)
        self.assertFalse(list(self.root.glob(".prepared-*")))
        return result

    def test_roundtrip_and_repeated_verification(self):
        self.bundle()
        self.run_bundle(True)
        self.assertEqual((self.root / "source/file").stat().st_mode & 0o777, 0o640)
        self.assertEqual((self.root / "source/link").read_bytes(), b"hello")
        self.run_bundle(True)
        (self.root / "invoked").unlink()
        (self.root / "source/file").write_bytes(b"changed")
        self.run_bundle()

    def test_existing_source_extra_file(self):
        self.bundle()
        self.run_bundle(True)
        (self.root / "invoked").unlink()
        (self.root / "source/extra").write_text("undeclared")
        self.run_bundle()

    def test_altered_archive(self):
        self.bundle()
        with (self.root / "prepared-source.tar").open("ab") as f:
            f.write(b"tamper")
        self.run_bundle()

    def test_altered_file(self):
        self.members[1] = ("source/file", tarfile.REGTYPE, b"wrong", "")
        self.bundle()
        self.run_bundle()

    def test_invalid_members(self):
        cases = [
            ("source/../escape", tarfile.REGTYPE, b"", ""),
            ("/absolute", tarfile.REGTYPE, b"", ""),
            ("source/file", tarfile.REGTYPE, b"hello", ""),
            ("source/unexpected", tarfile.REGTYPE, b"", ""),
        ]
        for member in cases:
            with self.subTest(member=member):
                original = self.members[:]
                self.members.append(member)
                self.bundle()
                self.run_bundle()
                self.assertFalse((self.root / "source").exists())
                self.members = original

    def test_missing_member(self):
        self.members.pop()
        self.bundle()
        self.run_bundle()

    def test_escaping_and_dangling_links(self):
        for target in ["../../outside", "/etc/passwd", "missing", "link"]:
            with self.subTest(target=target):
                self.members[-1] = ("source/link", tarfile.SYMTYPE, b"", target)
                self.files["source/link"] = "link:" + target
                self.bundle()
                self.run_bundle()

    def test_unsupported_types(self):
        for kind in [tarfile.LNKTYPE, tarfile.FIFOTYPE, tarfile.CHRTYPE, tarfile.GNUTYPE_SPARSE]:
            with self.subTest(kind=kind):
                self.members[1] = ("source/file", kind, b"", "source/link")
                self.bundle()
                self.run_bundle()

    def test_link_as_parent(self):
        self.members.append(("source/link/child", tarfile.REGTYPE, b"hello", ""))
        self.files["source/link/child"] = self.files["source/file"]
        self.bundle()
        self.run_bundle()

    def test_manifest_path_and_duplicate(self):
        for path in ["../outside", "source//file", ".", "/absolute", "source/file"]:
            with self.subTest(path=path):
                self.bundle()
                self.manifest["files"].append({"path": path, "identity": "dir:0755"})
                self.save()
                self.run_bundle()

    def test_oversized_pax_header(self):
        self.members.insert(0, ("pax", tarfile.XHDTYPE, b"x" * ((1 << 20) + 512), ""))
        self.bundle()
        result = self.run_bundle()
        self.assertIn("oversized", result.stderr)

    def test_existing_source_symlink(self):
        self.bundle()
        (self.root / "source").symlink_to("recipe")
        self.run_bundle()


if __name__ == "__main__":
    unittest.main()
