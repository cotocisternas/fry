#!/usr/bin/env python3
"""Bind local tool binaries to reviewed inputs and authenticated Go module sums."""

import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def requirements(path):
    return dict(re.findall(r"^\s*([^\s]+) (v[^\s]+)", path.read_text(), re.M))


def lint_graph(repo, upstream):
    local = requirements(repo / "tooling/native/golangci-lint/go.mod")
    release = requirements(upstream)
    for module, version in release.items():
        if module in local and local[module] != version:
            raise ValueError(f"golangci-lint dependency differs from upstream: {module}")
    if re.search(r"^(replace|exclude)\b", upstream.read_text(), re.M):
        raise ValueError("Review upstream golangci-lint replacement/exclusion directives")


def snapshot(repo, tool, go, binary, version, schema):
    manifest = repo / "tooling/native" / tool / "go.mod"
    text = manifest.read_text()
    package = re.search(r"^tool (\S+)$", text, re.M).group(1)
    pins = requirements(manifest)
    source = package.rsplit("/cmd/", 1)[0]
    info = json.loads(subprocess.check_output([go, "version", "-m", "-json", str(binary)]))
    if info["GoVersion"] != f"go{version}" or info["Path"] != package:
        raise ValueError("Compiler or command path differs from reviewed build")
    if info["Main"]["Path"] != source or info["Main"]["Version"] != pins[source]:
        raise ValueError("Source module differs from pinned release")
    settings = {item["Key"]: item["Value"] for item in info["Settings"]}
    expected = {"-trimpath": "true", "-buildmode": "exe", "-compiler": "gc",
                "CGO_ENABLED": "0", "GOOS": "linux"}
    host = subprocess.check_output([go, "env", "GOHOSTARCH"], text=True).strip()
    expected["GOARCH"] = host
    expected["GOAMD64" if host == "amd64" else "GOARM64"] = "v1" if host == "amd64" else "v8.0"
    if any(settings.get(key) != value for key, value in expected.items()):
        raise ValueError("Native build settings differ from reviewed recipe")
    if any(key.startswith("vcs.") for key in settings) or settings.get("GOEXPERIMENT"):
        raise ValueError("Unexpected VCS or experimental build settings")
    sums = {tuple(line.split()[:2]): line.split()[2]
            for line in manifest.with_suffix(".sum").read_text().splitlines()}
    for dep in [info["Main"], *info.get("Deps", [])]:
        if ("Replace" in dep or pins.get(dep["Path"]) != dep["Version"]
                or not dep.get("Sum")
                or sums.get((dep["Path"], dep["Version"])) != dep["Sum"]):
            raise ValueError(f"Unpinned module or checksum: {dep['Path']}")
    inputs = ["mise.toml", "mise.lock", "tooling/versions.env", "scripts/setup.sh",
              "scripts/toolchain.sh", "tooling/native/build.sh", "tooling/native/provenance.py",
              f"tooling/native/{tool}/go.mod", f"tooling/native/{tool}/go.sum"]
    return {"schema": int(schema), "inputs": {name: digest(repo / name) for name in inputs},
            "sha256": digest(binary), "build_info": info}


def main():
    action, root, *args = sys.argv[1:]
    repo = Path(root)
    if action == "lint-graph":
        lint_graph(repo, Path(args[0]))
        return
    tool, go, executable, version, schema = args
    if tool not in ("golangci-lint", "sqlc") or action not in ("record", "verify"):
        raise ValueError("Unknown native build/operation")
    binary = Path(executable)
    receipt = binary.with_name(binary.name + ".build.json")
    actual = snapshot(repo, tool, go, binary, version, schema)
    if action == "record":
        receipt.write_text(json.dumps(actual, indent=2, sort_keys=True) + "\n")
    elif json.loads(receipt.read_text()) != actual:
        raise ValueError("Native binary, build metadata or reviewed inputs changed")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, AttributeError, subprocess.SubprocessError) as error:
        sys.exit(f"toolchain: {error}; run scripts/setup.sh.")
