#!/usr/bin/env python3
"""Download authenticated distro packages into the isolated build workspace.

No host package installation occurs. The pinned builder's distro sources and
trusted APT keys verify the package checksums. Runtime files remain in the isolated build workspace.
"""
import hashlib
import json
import shutil
import subprocess
import sys
from pathlib import Path


def run(args, timeout=240):
    result = subprocess.run(args, capture_output=True, timeout=timeout)
    if result.returncode:
        # Do not expose configured repository credentials through diagnostics.
        raise RuntimeError("PDF runtime package operation failed: " + args[0])
    return result.stdout


def package(destination):
    destination = destination.resolve()
    if not str(destination).startswith("/work/") or destination.exists():
        raise ValueError("PDF runtime requires a new isolated /work directory")
    destination.mkdir()
    apt = destination / "apt"
    root = destination / "root"
    root.mkdir()
    for name in ("lists/partial", "archives/partial", "cache"):
        (apt / name).mkdir(parents=True)
    options = [
        "-o", "Dir::State::Lists=" + str(apt / "lists"),
        "-o", "Dir::State::status=/dev/null",
        "-o", "Dir::Cache::Archives=" + str(apt / "archives"),
        "-o", "Dir::Cache::pkgcache=" + str(apt / "cache/pkgcache"),
        "-o", "Dir::Cache::srcpkgcache=" + str(apt / "cache/srcpkgcache"),
        "-o", "Debug::NoLocking=1",
    ]
    run(["apt-get", *options, "update"])
    run(["apt-get", *options, "--no-install-recommends", "--download-only", "-y", "install", "poppler-utils"])
    debs = sorted((apt / "archives").glob("*.deb"))
    if not debs or len(debs) > 128 or sum(p.stat().st_size for p in debs) > 256 << 20:
        raise RuntimeError("PDF runtime package inventory exceeds its bound")
    receipt = {"distribution_packages": []}
    for deb in debs:
        receipt["distribution_packages"].append({"file": deb.name, "sha256": hashlib.sha256(deb.read_bytes()).hexdigest()})
        run(["dpkg-deb", "-x", str(deb), str(root)])
    # Ubuntu usrmerge packages place the interpreter under /usr/lib. ELF files
    # still name /lib64/ld-linux-x86-64.so.2. Preserve that interpreter path.
    interpreter = root / "usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"
    if not interpreter.is_file():
        interpreter = root / "lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"
    if not interpreter.is_file():
        raise RuntimeError("PDF runtime interpreter is missing")
    (root / "lib64").mkdir(exist_ok=True)
    target = root / "lib64/ld-linux-x86-64.so.2"
    if target.is_symlink():
        target.unlink()
    if target.resolve() != interpreter.resolve():
        shutil.copy2(interpreter, target)
    library_path = ":".join(str(root / p) for p in ("usr/lib/x86_64-linux-gnu", "lib/x86_64-linux-gnu", "usr/lib", "lib"))
    receipt["tools"] = {}
    for name in ("pdfinfo", "pdftotext", "pdftoppm"):
        tool = root / "usr/bin" / name
        if not tool.is_file():
            raise RuntimeError("PDF runtime tool is missing: " + name)
        # Verify using the downloaded libraries, not the builder's host libs.
        run([str(interpreter), "--library-path", library_path, str(tool), "-v"])
        receipt["tools"][name] = hashlib.sha256(tool.read_bytes()).hexdigest()
    (root / "pdf-runtime-receipt.json").write_text(json.dumps(receipt, sort_keys=True, indent=2) + "\n")
    # Keep parser tools, their shared libraries, fonts, and license notices.
    # Do not add an APT client or shell to the deployed application image.
    for item in sorted(root.rglob("*"), key=lambda p: len(p.parts), reverse=True):
        path = str(item.relative_to(root))
        if item.is_dir() and not item.is_symlink():
            try:
                item.rmdir()
            except OSError:
                pass
            continue
        allowed = (
            path in {"usr/bin/pdfinfo", "usr/bin/pdftotext", "usr/bin/pdftoppm", "pdf-runtime-receipt.json"}
            or path.startswith(("usr/lib/", "lib/", "lib64/", "usr/share/poppler/", "usr/share/fonts/", "usr/share/fontconfig/", "etc/fonts/"))
            or (path.startswith("usr/share/doc/") and path.endswith("/copyright"))
        )
        if not allowed:
            item.unlink()
    print(json.dumps({"pdf_runtime_packages": len(debs), "pdf_runtime_package_bytes": sum(p.stat().st_size for p in debs), "tools_verified": list(receipt["tools"])}), flush=True)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("Pass a new /work destination")
    package(Path(sys.argv[1]))
