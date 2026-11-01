#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026 gao-gao-zai and contributors.
"""Package an explicit, credential-free source inventory and Go vendor sources.

Run on the build host, after restoring the locked frontend dependencies. Never
archive the working directory wholesale or collect files from a running service.
"""
import argparse
import hashlib
import html
import io
import json
from pathlib import Path, PurePosixPath
import subprocess
import tarfile


ROOT_FILES = {
    "LICENSE", "NOTICE", "README.md", "DEPLOY.md", "go.mod", "go.sum", "legal.go", "legal_test.go",
    ".gitignore", ".dockerignore", ".env.example", "config.example.json",
    "docker-compose.yml", "docker-compose.postgres.yml", "Dockerfile",
    "Dockerfile.local", "Dockerfile.gateway", "login.sh", "credit.sh", "signin.sh",
    "react/package.json", "react/package-lock.json", "react/index.html",
    "react/vite.config.js", "react/vite.config.ts",
}
PREFIXES = ("cmd/", "internal/", "react/src/", "react/public/", "frontend/", "scripts/", "LICENSES/", ".github/")
# Reviewed runtime packages whose published npm tarballs omit a LICENSE file.
# Pin the fallback to the reviewed version; upgrades require another review.
NOTICE_OVERRIDES = {
    ("node_modules/@ant-design/icons-svg", "4.6.0"): "ant-design-icons-MIT.txt",
    ("node_modules/toggle-selection", "1.0.6"): "toggle-selection-MIT.txt",
}


def allowed(name):
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or "\\" in name:
        return False
    if any(p in {"node_modules", ".git", "__pycache__", "auths", "data", "backups"} for p in path.parts):
        return False
    if path.name.startswith(".env") and name != ".env.example":
        return False
    if path.suffix.lower() in {".pem", ".key", ".db", ".sqlite", ".sqlite3", ".log", ".pyc"}:
        return False
    return name in ROOT_FILES or name.startswith(PREFIXES)


def inventory(root, manifest=None):
    if manifest:
        names = Path(manifest).read_text(encoding="utf-8-sig").splitlines()
    elif (root / "SOURCE_FILES.txt").is_file():
        names = (root / "SOURCE_FILES.txt").read_text().splitlines()
    else:
        names = subprocess.check_output(
            ["git", "ls-files", "--cached", "--others", "--exclude-standard"], cwd=root, text=True
        ).splitlines()
    # Vite changes hashed filenames. Include this build's assets, not stale names
    # from the pre-build inventory. Notices may also be generated during packaging.
    for directory in ("frontend", "LICENSES"):
        names += [p.relative_to(root).as_posix() for p in (root / directory).rglob("*") if p.is_file()]
    result = []
    for name in sorted(set(names)):
        if not allowed(name):
            continue
        path = root / name
        if path.is_symlink() or not path.resolve().is_relative_to(root):
            raise ValueError(f"source inventory escapes project: {name}")
        if path.is_file():
            result.append(name)
    for required in ("LICENSE", "NOTICE", "go.mod", "react/package-lock.json", "cmd/server/main.go", "scripts/build-release.sh"):
        if required not in result:
            raise ValueError(f"source inventory missing: {required}")
    return result


def license_files(directory):
    # Include dependency-local notices, but do not recursively traverse nested
    # node_modules belonging to other packages or follow directory symlinks.
    for path in sorted(directory.iterdir()):
        if path.is_file() and path.name.lower().startswith(("license", "licence", "copying", "notice", "copyright", "authors")):
            yield path


def dependency_notices(root, vendor):
    parts = ["Third-party dependency notices — generated from this release.\n",
             "Dependency licenses remain applicable; they are not replaced by the project AGPL grant.\n",
             "Go vendor sources and npm production dependencies are included in source.tar.gz.\n",
             "npm dev-only build tools are not distributed as part of the application.\n"]
    missing = []
    modules = (vendor / "modules.txt").read_text()
    for line in modules.splitlines():
        if not line.startswith("# ") or " => " in line:
            continue
        fields = line[2:].split()
        if len(fields) < 2 or not fields[1].startswith("v"):
            continue
        module, version = fields[:2]
        if not (vendor / module).is_dir():
            continue  # Explicit module with no imported packages is not vendored.
        parts.append(f"\n===== Go: {module} {version} =====\n")
        files = list(license_files(vendor / module)) if (vendor / module).is_dir() else []
        if not files:
            missing.append(module)
        for path in files:
            parts.append(f"\n--- {path.name} ---\n" + path.read_text(errors="replace") + "\n")
    lock = json.loads((root / "react/package-lock.json").read_text())
    for package, meta in sorted(lock["packages"].items()):
        if not package or meta.get("dev"):
            continue
        directory = root / "react" / package
        if not directory.is_dir():
            # Platform-specific optional packages may legitimately be absent.
            if meta.get("optional"):
                continue
            raise ValueError(f"locked dependency not installed: {package}")
        parts.append(f"\n===== npm: {package} {meta.get('version', '')} ({meta.get('license', 'see package')}) =====\n")
        files = list(license_files(directory))
        override = NOTICE_OVERRIDES.get((package, meta.get("version")))
        if not files and override:
            files = [root / "LICENSES" / override]
        if not files:
            # Some upstream distributions put the complete grant in README.
            for readme in directory.glob("*README*"):
                text = readme.read_text(errors="replace")
                marker = text.lower().find("## license")
                if marker >= 0 and "permission is hereby granted" in text[marker:].lower():
                    parts.append(html.unescape(text[marker:]) + "\n")
                    break
            else:
                missing.append(package)
        for path in files:
            parts.append(f"\n--- {path.name} ---\n" + path.read_text(errors="replace") + "\n")
    if missing:
        raise ValueError("dependency notices require manual review: " + ", ".join(missing))
    (root / "LICENSES/dependencies.txt").write_text("".join(parts), encoding="utf-8")


def add_bytes(archive, name, content):
    info = tarfile.TarInfo(name)
    info.size, info.mode, info.mtime = len(content), 0o644, 0
    archive.addfile(info, io.BytesIO(content))


def package(root, output, vendor, manifest=None):
    names = inventory(root, manifest)
    fingerprints = {name: hashlib.sha256((root / name).read_bytes()).hexdigest() for name in names}
    output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(output, "w:gz", dereference=False) as archive:
        for name in names:
            archive.add(root / name, arcname=name, recursive=False)
        for path in sorted(vendor.rglob("*")):
            if path.is_symlink() or not path.resolve().is_relative_to(vendor.resolve()):
                raise ValueError("vendor source contains a symlink")
            if path.is_file():
                archive.add(path, arcname="vendor/" + path.relative_to(vendor).as_posix(), recursive=False)
        lock = json.loads((root / "react/package-lock.json").read_text())
        for name, meta in sorted(lock["packages"].items()):
            if not name or meta.get("dev"):
                continue
            directory = root / "react" / name
            if not directory.is_dir() and meta.get("optional"):
                continue
            for path in sorted(directory.rglob("*")):
                if path.is_symlink() or not path.resolve().is_relative_to(root / "react/node_modules"):
                    raise ValueError("npm production source contains a symlink")
                if path.is_file():
                    archive.add(path, arcname=path.relative_to(root).as_posix(), recursive=False)
        add_bytes(archive, "SOURCE_FILES.txt", ("\n".join(names) + "\n").encode())
        add_bytes(archive, "SOURCE_MANIFEST.json", json.dumps({"license": "AGPL-3.0-only", "sha256": fingerprints}, indent=2).encode())
    return hashlib.sha256(output.read_bytes()).hexdigest()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--vendor-dir", type=Path, required=True)
    parser.add_argument("--manifest", type=Path)
    args = parser.parse_args()
    root, vendor = args.root.resolve(), args.vendor_dir.resolve()
    if not (vendor / "modules.txt").is_file():
        raise SystemExit("missing Go vendor sources; run go mod vendor on the build host first")
    dependency_notices(root, vendor)
    digest = package(root, args.output.resolve(), vendor, args.manifest)
    print(f"{digest}  {args.output.name}")
