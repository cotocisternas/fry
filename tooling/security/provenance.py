"""Local scanner receipts bind executable bytes to source pins and compiler."""

import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys


TOOLS = {
    "actionlint": ("github.com/rhysd/actionlint/cmd/actionlint", "github.com/rhysd/actionlint"),
    "govulncheck": ("golang.org/x/vuln/cmd/govulncheck", "golang.org/x/vuln"),
    "gitleaks": ("github.com/zricethezav/gitleaks/v8", "github.com/zricethezav/gitleaks/v8"),
}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def snapshot(repo, go, directory):
    manifest = repo / "tooling/security/scanners/go.mod"
    pins = dict(re.findall(r"^\s*(\S+) (v\S+)", manifest.read_text(), re.M))
    sums = {tuple(line.split()[:2]): line.split()[2]
            for line in manifest.with_suffix(".sum").read_text().splitlines()}
    compiler = subprocess.check_output([go, "env", "GOVERSION"], text=True).strip()
    arch = subprocess.check_output([go, "env", "GOHOSTARCH"], text=True).strip()
    binaries = {}
    for name, (package, module) in TOOLS.items():
        binary = directory / name
        info = json.loads(subprocess.check_output([go, "version", "-m", "-json", str(binary)]))
        if (info["GoVersion"] != compiler or info["Path"] != package
                or info["Main"]["Path"] != module or info["Main"]["Version"] != pins[module]):
            raise ValueError("Scanner compiler, source module or version differs from pins")
        settings = {s["Key"]: s["Value"] for s in info["Settings"]}
        expected = {"-trimpath": "true", "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch,
                    "GOAMD64" if arch == "amd64" else "GOARM64": "v1" if arch == "amd64" else "v8.0"}
        if any(settings.get(k) != v for k, v in expected.items()):
            raise ValueError("Scanner build settings differ from recipe")
        if any(k.startswith("vcs.") for k in settings) or settings.get("GOEXPERIMENT"):
            raise ValueError("Unexpected scanner VCS or experiment metadata")
        for dep in [info["Main"], *info.get("Deps", [])]:
            if ("Replace" in dep or pins.get(dep["Path"]) != dep["Version"]
                    or not dep.get("Sum") or sums.get((dep["Path"], dep["Version"])) != dep["Sum"]):
                raise ValueError("Scanner contains an unpinned dependency")
        binaries[name] = {"sha256": digest(binary), "build_info": info}
    inputs = ["mise.toml", "mise.lock", "scripts/security/setup.sh",
              "tooling/security/provenance.py", "tooling/security/scanners/go.mod",
              "tooling/security/scanners/go.sum"]
    return {"schema": 1, "inputs": {p: digest(repo / p) for p in inputs}, "binaries": binaries}


def verify(repo, go, directory):
    receipt = json.loads((directory / "build.json").read_text())
    if receipt != snapshot(repo, go, directory):
        raise ValueError("Security scanner bytes or build inputs changed; run scripts/security/setup.sh")


if __name__ == "__main__":
    action, root, go, destination = sys.argv[1:]
    repo, directory = Path(root), Path(destination)
    if action == "record":
        (directory / "build.json").write_text(json.dumps(snapshot(repo, go, directory), indent=2) + "\n")
    elif action == "verify":
        verify(repo, go, directory)
    else:
        raise ValueError("Unknown scanner receipt operation")
