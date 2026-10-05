#!/usr/bin/env python3
"""Export the public source of Mailie from an explicit allowlist, without Git history.

Only ROOTS and FILES are exported. Everything else at the top of the checkout
must be commercial/ (the private cloud a development checkout nests there), a
known local artifact, or, in a Git checkout, something Git ignores; any other
entry fails the export, so a new top-level directory is never dropped without
a decision. The export refuses symlinks, environment files other than
.env.example, local settings and secrets (*.local.*, .npmrc), local databases,
anything under commercial/, and any file whose path or bytes hold a forbidden
string. The one local file it skips inside a root, without reading it, is
deploy/.env: the environment file deploy/compose.yaml reads, which
docs/self-hosting.md has an operator create there and Git ignores.

In a Git checkout (.git at the root) the export is exactly the files Git
tracks under ROOTS and FILES, read from the working tree. A file there that
Git ignores is refused as local, one Git does not track yet is refused until
it is added or removed, and a tracked file the walk would skip (EXCLUDED,
SKIPPED_SUFFIXES) is refused rather than dropped. A directory without .git,
such as an extracted export, is exported as it is on disk.

The forbidden strings are private: they name what the export keeps out, so
they live in commercial/public-export-forbidden.txt (or the file given with
--forbidden), never in this public script. A checkout with commercial/ must
have that list. Without commercial/ (the public repository itself) only the
generic strings below are checked.

The archive is deterministic: sorted paths, uid/gid 0, no owner names, mtime
0, modes 0644 or 0755, and a gzip header with no name or time.
"""
import argparse
import fnmatch
import gzip
import io
import os
import pathlib
import subprocess
import sys
import tarfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
ROOTS = ("cmd", "internal", "it", "web", "docs", "scripts", "deploy", ".github")
FILES = (
    "go.mod", "go.sum", "README.md", "LICENSE", "NOTICE", "CONTRIBUTING.md", "SECURITY.md", "Makefile",
    ".env.example", ".gitignore", ".dockerignore", ".golangci.yml", ".gitleaks.toml", "CLAUDE.md",
)
PRIVATE = "commercial"
# Top-level entries that are local and never exported (.gitignore): build
# output, the binary `go build ./cmd/mailserver` leaves at the root, make
# cover's profile, local runtime state, the development .env, Claude Code's
# local instructions and settings, editors' project settings, Finder's files.
LOCAL_ARTIFACTS = {
    "bin", "dist", "mailserver", "coverage.out", "data", ".env", "CLAUDE.local.md", ".claude", ".vscode",
    ".idea", ".DS_Store", ".git",
}
# Local files inside a root that are expected there and never exported nor
# read: the Compose stack's environment file, which holds the credential key.
# Only while Git does not track them: a tracked one is refused.
LOCAL_IN_ROOTS = {"deploy/.env"}
# Path parts never exported from inside a root: dependencies, builds, caches.
EXCLUDED = {"node_modules", "dist", ".git", ".DS_Store", "__pycache__", "bin", "coverage", "test-results"}
# Build and test outputs inside a root, skipped like EXCLUDED.
SKIPPED_SUFFIXES = (".pyc", ".out", ".test")
# Local state that must not be lying in a public root at all: refused.
REFUSED_SUFFIXES = (".db", ".db-shm", ".db-wal")
# Local settings and secrets (.gitignore's "Secrets"): refused by name, so
# that they stay out even where there is no .git to ask.
REFUSED_NAMES = (".npmrc",)
REFUSED_PATTERNS = ("*.local.*",)
# Safe to publish: a macOS home directory is never part of the source. Spelled
# in pieces, so that this file, which is exported too, does not hold it.
GENERIC_FORBIDDEN = ("/" + "Users" + "/",)
DEFAULT_FORBIDDEN_LIST = ROOT / PRIVATE / "public-export-forbidden.txt"


class Refused(Exception):
    pass


class Git:
    """What Git says about ROOT, when ROOT is a Git checkout."""

    def __init__(self):
        self.tracked = set(self._paths("ls-files", "-z", "--cached"))

    @staticmethod
    def open():
        if not (ROOT / ".git").exists():
            print("no .git here: exporting what is on disk", file=sys.stderr)
            return None
        return Git()

    @staticmethod
    def _run(*args, stdin=None):
        try:
            return subprocess.run(["git", "-C", str(ROOT), *args], input=stdin, capture_output=True)
        except OSError as e:
            raise Refused(f"this is a Git checkout, but git cannot run: {e}")

    def _paths(self, *args, stdin=None, ok=(0,)):
        done = self._run(*args, stdin=stdin)
        if done.returncode not in ok:
            raise Refused(f"git {args[0]} failed: {done.stderr.decode(errors='replace').strip()}")
        return [p for p in done.stdout.decode().split("\0") if p]

    def ignored(self, paths):
        """The paths Git ignores, as given. A directory is given with a trailing slash."""
        if not paths:
            return set()
        # check-ignore exits 1 when it ignores none of them.
        return set(self._paths("check-ignore", "-z", "--stdin", stdin="".join(p + "\0" for p in paths).encode(),
                                ok=(0, 1)))

    def refusals(self, walked):
        """What the walk found that Git does not track, and what Git tracks that the walk skipped."""
        found = {p.as_posix() for p in walked}
        untracked = sorted(found - self.tracked)
        ignored = self.ignored(untracked)
        out = {}
        for path in untracked:
            if path in ignored:
                out[path] = f"refusing {path}: Git ignores it as local; move it out of the public roots"
            else:
                out[path] = f"refusing {path}: Git does not track it; git add it, or remove it"
        for path in sorted(self.tracked - found):
            public = path in FILES or ("/" in path and path.split("/", 1)[0] in ROOTS)
            if public and os.path.lexists(ROOT / path):
                out[path] = (f"refusing to drop {path}: Git tracks it, but its name is in EXCLUDED, "
                             "SKIPPED_SUFFIXES or LOCAL_IN_ROOTS of scripts/export-public.py; rename it, or untrack it")
        return out


def forbidden_strings(path):
    """The generic strings plus the private list at path, when there is one."""
    strings = [s.encode() for s in GENERIC_FORBIDDEN]
    if path is None:
        if (ROOT / PRIVATE).exists():
            path = DEFAULT_FORBIDDEN_LIST
            if not path.is_file():
                raise Refused(f"{path.relative_to(ROOT)} is missing: a checkout with {PRIVATE}/ must have the private list")
        else:
            print(f"no {PRIVATE}/ here: checking the generic strings only", file=sys.stderr)
            return strings
    if not path.is_file():
        raise Refused(f"the forbidden list {path} is missing")
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and line.encode() not in strings:
            strings.append(line.encode())
    return strings


def check_top_level(git):
    allowed = set(ROOTS) | set(FILES) | {PRIVATE} | LOCAL_ARTIFACTS
    unknown = sorted(p.name for p in ROOT.iterdir() if p.name not in allowed)
    if git is not None and unknown:
        # Whatever Git ignores at the top is local by declaration: it can
        # never be part of the public repository.
        given = {name + "/" if (ROOT / name).is_dir() and not (ROOT / name).is_symlink() else name: name
                 for name in unknown}
        ignored = {given[p] for p in git.ignored(sorted(given))}
        unknown = [name for name in unknown if name not in ignored]
    if unknown:
        raise Refused(
            "top-level entries neither exported nor known to stay out: " + ", ".join(unknown)
            + "\nto publish one, add it to ROOTS or FILES in scripts/export-public.py; if it is local,"
            + " ignore it in .gitignore and add it to LOCAL_ARTIFACTS there; otherwise remove it"
        )


def check_file(path, relative):
    if relative.parts[0] == PRIVATE or (ROOT / PRIVATE) in path.resolve().parents:
        raise Refused(f"refusing private source: {relative}")
    name = path.name
    if name.startswith(".env") and name != ".env.example":
        raise Refused(f"refusing environment file: {relative}")
    if name in REFUSED_NAMES or any(fnmatch.fnmatchcase(name, p) for p in REFUSED_PATTERNS):
        raise Refused(f"refusing local settings or secrets: {relative}")
    if name.endswith(REFUSED_SUFFIXES):
        raise Refused(f"refusing local database: {relative}")


def walk(root):
    """Every file under root, refusing symlinks, sorted, without EXCLUDED parts."""
    top = ROOT / root
    if top.is_symlink():
        raise Refused(f"refusing symlink: {root}")
    if not top.is_dir():
        raise Refused(f"allowlisted root missing: {root}")
    for directory, dirs, files in os.walk(top, followlinks=False):
        here = pathlib.Path(directory)
        dirs[:] = sorted(d for d in dirs if d not in EXCLUDED)
        for name in dirs:
            if (here / name).is_symlink():
                raise Refused(f"refusing symlink: {(here / name).relative_to(ROOT)}")
        for name in sorted(files):
            path = here / name
            relative = path.relative_to(ROOT)
            if relative.as_posix() in LOCAL_IN_ROOTS:
                continue
            if path.is_symlink():
                raise Refused(f"refusing symlink: {relative}")
            if name in EXCLUDED or name.endswith(SKIPPED_SUFFIXES):
                continue
            if not path.is_file():
                raise Refused(f"refusing a file that is not regular: {relative}")
            yield path


def public_files():
    for root in ROOTS:
        yield from walk(root)
    for name in FILES:
        path = ROOT / name
        if path.is_symlink():
            raise Refused(f"refusing symlink: {name}")
        if not path.is_file():
            raise Refused(f"allowlisted file missing: {name}")
        yield path


def scan(path, relative, forbidden):
    data = path.read_bytes()
    where = relative.as_posix().encode()
    held = [i + 1 for i, s in enumerate(forbidden) if s in data or s in where]
    if held:
        # By number, not by value: the strings are private, and CI logs are not.
        raise Refused(f"refusing {relative}: it holds forbidden string(s) #{', #'.join(map(str, held))}")
    return data


def write_archive(output, entries):
    output.parent.mkdir(parents=True, exist_ok=True)
    partial = output.with_name(output.name + ".partial")
    with open(partial, "wb") as raw, \
            gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as gz, \
            tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for relative, data, executable in entries:
            info = tarfile.TarInfo(relative.as_posix())
            info.size = len(data)
            info.mode = 0o755 if executable else 0o644
            info.mtime = 0
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            tar.addfile(info, io.BytesIO(data))
    os.replace(partial, output)


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("--forbidden", type=pathlib.Path,
                        help=f"the private list of forbidden strings (default: {PRIVATE}/public-export-forbidden.txt)")
    args = parser.parse_args()
    # Structural problems stop at once; a file's are collected, so one run
    # names every file to fix.
    refusals, entries = [], []
    try:
        forbidden = forbidden_strings(args.forbidden)
        git = Git.open()
        check_top_level(git)
        files = [path.relative_to(ROOT) for path in public_files()]
        by_git = git.refusals(files) if git is not None else {}
        for relative in files:
            path = ROOT / relative
            try:
                if relative.as_posix() in by_git:
                    raise Refused(by_git.pop(relative.as_posix()))
                check_file(path, relative)
                data = scan(path, relative, forbidden)
            except Refused as e:
                refusals.append(str(e))
                continue
            entries.append((relative, data, os.stat(path).st_mode & 0o111 != 0))
        # What is left is what Git tracks and the walk skipped.
        refusals.extend(by_git[p] for p in sorted(by_git))
    except Refused as e:
        refusals.append(str(e))
    entries.sort(key=lambda e: e[0].as_posix())
    names = [e[0].as_posix() for e in entries]
    if len(set(names)) != len(names):
        refusals.append("a path is exported twice")
    if refusals:
        for r in refusals:
            print(f"export-public: {r}", file=sys.stderr)
        print("export-public: nothing written", file=sys.stderr)
        return 1
    for name in names:
        print(name)
    write_archive(args.output, entries)
    print(f"Public source: {args.output} ({len(entries)} files)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
