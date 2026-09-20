#!/usr/bin/env python3
"""Verify and rebuild a Pekit corresponding-source bundle without its checkout."""
import gzip
import hashlib
import json
import os
import pathlib
import shutil
import stat
import sys
import tarfile
import tempfile

MAX_ARCHIVE = 4 << 30
MAX_MANIFEST = 64 << 20
CHUNK = 1 << 20
root = pathlib.Path(__file__).resolve().parent


def reject(message):
    raise ValueError(message)


def relative_path(value):
    path = pathlib.PurePosixPath(value)
    if (not value or value == "." or path.is_absolute() or path.as_posix() != value
            or ".." in path.parts or len(path.parts) > 256
            or len(value.encode()) > 4096
            or any(len(part.encode()) > 255 for part in path.parts)):
        reject("invalid source bundle path: " + value)
    return path


def verify_contained(base, path):
    """Permit internal dangling fixtures while rejecting unsafe link traversal."""
    pending = list(path.relative_to(base).parts)
    parts, hops = [], 0
    while pending:
        part = pending.pop(0)
        if part in ("", "."):
            continue
        if part == "..":
            if not parts:
                reject("source link escapes")
            parts.pop()
            continue
        candidate = base.joinpath(*parts, part)
        try:
            info = candidate.lstat()
        except FileNotFoundError:
            info = None
        if info is not None and stat.S_ISLNK(info.st_mode):
            hops += 1
            target = os.readlink(candidate)
            if hops > 40 or pathlib.PurePosixPath(target).is_absolute():
                reject("absolute or cyclic source link")
            pending = target.split("/") + pending
            continue
        if info is not None and pending and not stat.S_ISDIR(info.st_mode):
            reject("source link parent is not a directory")
        parts.append(part)


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(CHUNK), b""):
            h.update(block)
    return h.hexdigest()


def parse_identity(identity):
    kind, value = identity.split(":", 1)
    if kind == "link":
        if not value or "\0" in value or pathlib.PurePosixPath(value).is_absolute():
            reject("invalid source link")
        return kind, value
    mode = int(value if kind == "dir" else kind, 8)
    if mode & ~0o777:
        reject("invalid source mode")
    if kind != "dir" and (len(value) != 64 or any(c not in "0123456789abcdef" for c in value)):
        reject("invalid source hash")
    return ("dir" if kind == "dir" else "file"), mode


def unpack_prepared(manifest, identities):
    name = manifest.get("prepared_archive")
    if name != {3: "prepared-source.tar", 4: "prepared-source.tar.gz"}.get(manifest.get("schema")) or name not in identities:
        reject("invalid prepared source archive")
    archive = root / name
    if not stat.S_ISREG(archive.lstat().st_mode) or archive.stat().st_size > MAX_ARCHIVE:
        reject("invalid prepared source archive size or type")
    kind, _ = parse_identity(identities[name])
    if kind != "file" or digest(archive) != identities[name].split(":", 1)[1]:
        reject("prepared source archive changed")
    expected = {p: i for p, i in identities.items() if p == "source" or p.startswith("source/")}
    if expected.get("source", "").split(":", 1)[0] != "dir":
        reject("missing prepared source directory")
    for path in expected:
        for parent in pathlib.PurePosixPath(path).parents:
            if str(parent) != "." and not expected.get(str(parent), "").startswith("dir:"):
                reject("prepared source parent is not a declared directory")
    if (root / "source").exists() or (root / "source").is_symlink():
        if not stat.S_ISDIR((root / "source").lstat().st_mode):
            reject("prepared source is not a directory")
        actual = {"source"}
        for directory, dirs, files in os.walk(root / "source", followlinks=False, onerror=lambda error: reject(str(error))):
            actual.update(str((pathlib.Path(directory) / name).relative_to(root)) for name in dirs + files)
        if actual != set(expected):
            reject("existing prepared source has missing or unexpected members")
        return

    # tarfile reads extended headers in one allocation. Bound every read before
    # parsing, including PAX/GNU long-name headers, without extract/extractall.
    class BoundedReader:
        def __init__(self, stream):
            self.stream = stream
            self.position = 0

        def read(self, size=-1):
            if size < 0 or size > CHUNK:
                reject("oversized prepared source tar header")
            data = self.stream.read(min(size, MAX_ARCHIVE - self.position + 1))
            self.position += len(data)
            if self.position > MAX_ARCHIVE:
                reject("prepared source tar stream exceeds size limit")
            return data

        def tell(self):
            return self.position

        def seek(self, offset, whence=0):
            target = offset if whence == 0 else self.position + offset if whence == 1 else -1
            if target < self.position or target > MAX_ARCHIVE:
                reject("invalid prepared source tar seek")
            while self.position < target:
                if not self.read(min(CHUNK, target - self.position)):
                    reject("truncated prepared source tar stream")
            return self.position

    temporary = pathlib.Path(tempfile.mkdtemp(prefix=".prepared-", dir=root))
    try:
        seen, links, total = set(), [], 0
        opener = gzip.open if manifest["schema"] == 4 else open
        with opener(archive, "rb") as stream:
            bounded = BoundedReader(stream)
            with tarfile.open(fileobj=bounded, mode="r:") as tar:
                for member in tar:
                    path = member.name.rstrip("/") if member.isdir() else member.name
                    relative_path(path)
                    if path not in expected or path in seen:
                        reject("unexpected or duplicate prepared source member: " + path)
                    seen.add(path)
                    kind, _ = parse_identity(expected[path])
                    destination = temporary / path
                    if member.sparse is not None or member.size < 0:
                        reject("sparse or invalid prepared source member")
                    total += member.size
                    if total > MAX_ARCHIVE:
                        reject("prepared source exceeds size limit")
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    if member.isdir() and kind == "dir" and member.size == 0:
                        destination.mkdir(exist_ok=True)
                    elif member.issym() and kind == "link" and member.size == 0:
                        if member.linkname != expected[path][5:]:
                            reject("prepared source link changed")
                        links.append((destination, member.linkname))
                    elif member.isfile() and kind == "file":
                        h = hashlib.sha256()
                        with tar.extractfile(member) as source, destination.open("xb") as target:
                            for block in iter(lambda: source.read(CHUNK), b""):
                                h.update(block)
                                target.write(block)
                        if h.hexdigest() != expected[path].split(":", 1)[1]:
                            reject("prepared source input changed: " + path)
                    else:
                        reject("unsupported prepared source member: " + path)
                    tar.members.clear()
            # Read through padding and gzip trailers, enforcing the decoded
            # stream cap even after tar's end marker and validating gzip CRC.
            while bounded.read(CHUNK):
                pass
        if seen != set(expected):
            reject("missing prepared source members")
        for destination, link in links:
            destination.symlink_to(link)
        for destination, _ in links:
            verify_contained(temporary / "source", destination)
        os.rename(temporary / "source", root / "source")
    finally:
        shutil.rmtree(temporary)


def main():
    with (root / "build-inputs.json").open("rb") as stream:
        data = stream.read(MAX_MANIFEST + 1)
    if len(data) > MAX_MANIFEST:
        reject("source identity manifest exceeds size limit")
    manifest = json.loads(data)
    if manifest.get("schema") not in (2, 3, 4):
        reject("unsupported source bundle schema")
    identities = {}
    for entry in manifest["files"]:
        name = entry["path"]
        relative_path(name)
        if name in identities:
            reject("duplicate source bundle path")
        parse_identity(entry["identity"])
        identities[name] = entry["identity"]
    if manifest["schema"] in (3, 4):
        unpack_prepared(manifest, identities)
    restore_modes = []
    for name, identity in identities.items():
        path = root / name
        verify_contained(root, path)
        info = path.lstat()
        kind, value = parse_identity(identity)
        if stat.S_ISLNK(info.st_mode) and kind == "link":
            actual = "link:" + os.readlink(path)
        elif stat.S_ISDIR(info.st_mode) and kind == "dir":
            actual = identity
            restore_modes.append((path, value))
        elif stat.S_ISREG(info.st_mode) and kind == "file":
            actual = identity.split(":", 1)[0] + ":" + digest(path)
            restore_modes.append((path, value))
        else:
            reject("unsupported source bundle file: " + name)
        if actual != identity:
            reject("source bundle input changed: " + name)
    # Transport modes need not equal source modes. Verify all content first.
    for path, mode in sorted(restore_modes, key=lambda item: len(item[0].parts), reverse=True):
        os.chmod(path, mode)
    command = sys.argv[1] if len(sys.argv) > 1 else "test"
    if command not in ("build", "test", "package"):
        reject("usage: rebuild.py [build|test|package] [additional Pekit flags]")
    recipe = root / relative_path(manifest["recipe"])
    if not recipe.resolve().is_relative_to(root):
        reject("recipe escapes source bundle")
    args = ["pekit", "--recipe", str(recipe), command, "--local=" + str(root / "source")]
    if manifest.get("source_version"):
        args += ["--version", manifest["source_version"]]
    environment = os.environ.get("PEKIT_REBUILD_ENV", manifest.get("environment", ""))
    if environment:
        args += ["--env", environment]
    os.execvp(args[0], args + sys.argv[2:])


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, EOFError, RuntimeError, tarfile.TarError) as error:
        sys.exit("source bundle verification failed: " + str(error))
