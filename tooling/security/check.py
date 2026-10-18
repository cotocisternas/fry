"""Native PR security gate. Only summary.json is suitable for CI upload."""

from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import urllib.request

from policy import (PolicyError, go_allowed, govuln_result, json_stream,
                    load_policy, npm_result, osv_result, require, secret_result)
from provenance import TOOLS, verify

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from scripts.lib.testdeps import capture


REPO = Path(__file__).resolve().parents[2]
OUTPUT = REPO / ".artifacts/security"
RAW = OUTPUT / "private"
SCANNERS = REPO / ".artifacts/security-tools" / os.uname().machine / "bin"


def write_json(path, data):
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def command(args, cwd, timeout=900):
    # Captured output must never escape in an exception or CI console message.
    previous = Path.cwd()
    try:
        os.chdir(cwd)
        result = capture([str(a) for a in args], timeout=timeout)
    except (OSError, subprocess.SubprocessError):
        raise PolicyError("Scanner failed or exceeded its deadline") from None
    finally:
        os.chdir(previous)
    return result.returncode, result.stdout.decode("utf-8", errors="strict"), result.stderr.decode("utf-8", errors="strict")


def run(args, cwd, name):
    code, stdout, stderr = command(args, cwd)
    (RAW / (name + ".stdout")).write_text(stdout)
    (RAW / (name + ".stderr")).write_text(stderr)
    require(code == 0, "Command failed; inspect private local diagnostics")
    return stdout


def http_json(url, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    # TLS verification remains enabled; no cached or offline vulnerability fallback.
    with urllib.request.urlopen(request, timeout=60) as response:
        require(response.status == 200, "Security service request failed")
        return json.load(response)


def source_paths():
    output = subprocess.check_output(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=REPO)
    paths = sorted({os.fsdecode(p) for p in output.split(b"\0") if p})
    require(paths, "No Git source files found")
    for path in paths:
        parts = Path(path).parts
        require(not Path(path).is_absolute() and ".." not in parts, "Unsafe source path")
        require(".artifacts" not in parts and "node_modules" not in parts,
                "Tracked artifact cache must be removed from Git")
        require((REPO / path).is_symlink() or (REPO / path).is_file(),
                "Missing tracked file or unsupported submodule")
    return paths


def materialize(paths, destination):
    inventory = {}
    for path in paths:
        source = REPO / path
        # Git versions link text, not the external target. Tracked targets are scanned separately.
        data = os.fsencode(os.readlink(source)) if source.is_symlink() else source.read_bytes()
        target = destination / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        inventory[path] = hashlib.sha256(data).hexdigest()
    return inventory


def secrets(policy, snapshot, scratch):
    ignore = scratch / "empty.ignore"
    ignore.write_text("")
    common = ["--config", REPO / "tooling/security/gitleaks.toml", "--ignore-gitleaks-allow",
              "--gitleaks-ignore-path", ignore, "--exit-code=23", "--report-format=json",
              "--report-path=-", "--no-banner", "--no-color", "--log-level=error",
              "--max-decode-depth=5", "--max-archive-depth=2", "--max-target-megabytes=0", "--timeout=300"]
    shallow = run(["git", "rev-parse", "--is-shallow-repository"], REPO, "git-depth").strip()
    require(shallow == "false", "Secret history scan requires fetch-depth: 0")
    counts = {}
    for name, mode, cwd, flags in [("tree", "dir", snapshot, ["."]),
                                    ("history", "git", REPO, ["--log-opts=--all --full-history", "."])]:
        # Secret bytes exist only in this pipe's memory. Never persist raw JSON or stderr.
        code, stdout, stderr = command([SCANNERS / "gitleaks", mode, *common, *flags], cwd)
        require(not stderr.strip(), "Secret scanner reported an error")
        findings = secret_result(json.loads(stdout), code, policy)
        write_json(RAW / f"secrets-{name}.json", findings)
        require(all(f["accepted"] for f in findings), "Unaccepted secret finding; inspect sanitized local locations")
        counts[name] = {"findings": len(findings), "accepted": len(findings)}
    return counts


def inventories(policy):
    imported, pairs = {}, {}
    scanner_packages = [package for package, _ in TOOLS.values()]
    for scope in policy["graph_scopes"]:
        name = "root" if scope == "." else scope.replace("/", "-")
        directory = REPO / scope
        if scope in policy["source_scopes"] or scope == "tooling/security/scanners":
            packages = scanner_packages if scope == "tooling/security/scanners" else ["./..."]
            text = run(["go", "list", "-deps", "-test", "-json", *packages], directory, name + "-packages")
            loaded = json_stream(text)
            require(all(p.get("ImportPath") and not p.get("Error") and not p.get("DepsErrors") for p in loaded),
                    "Incomplete Go test dependency inventory")
            imported[scope] = {p["ImportPath"].split(" ")[0] for p in loaded}
        modules = json_stream(run(["go", "list", "-m", "-json", "all"], directory, name + "-modules"))
        for module in modules:
            if module.get("Main"):
                continue
            effective = module.get("Replace", module)
            path, version = effective.get("Path"), effective.get("Version")
            if not version:
                # Local mail forks retain their documented upstream-version queries.
                require(module.get("Path") in policy["fork_versions"], "Unreviewed local Go replacement")
                path, version = module["Path"], policy["fork_versions"][module["Path"]]
                require(Path(effective["Dir"]).resolve() in {REPO / "third_party/go-imap", REPO / "third_party/go-smtp"},
                        "Local fork replacement points outside reviewed source")
            require(path and version, "Incomplete Go module identity")
            pairs.setdefault((path, version), set()).add(scope)
    return imported, pairs


def graph_scan(policy, imported, pairs, go_version):
    pairs.setdefault(("stdlib", go_version.removeprefix("go")), set()).update(policy["graph_scopes"])
    ordered = sorted(pairs)
    queries = [{"package": {"ecosystem": "Go", "name": module}, "version": version}
               for module, version in ordered]
    response = http_json("https://api.osv.dev/v1/querybatch", {"queries": queries})
    write_json(RAW / "osv.json", response)
    findings = osv_result(response, ordered)
    for module, version, advisory in findings:
        require(all(go_allowed(policy, scope, advisory, module, version, imported)
                    for scope in pairs[(module, version)]), "Unaccepted OSV graph advisory")
    return {"module_versions": len(ordered), "findings": len(findings), "accepted": len(findings)}


def binary_targets(policy):
    targets = {}
    native = REPO / ".artifacts/native-toolchain" / os.uname().machine / "bin"
    for scope in policy["native_scopes"]:
        tool = Path(scope).name
        require(tool in ("golangci-lint", "sqlc"), "Unreviewed native scanner target")
        targets["native:" + tool] = native / tool
    require({"native:golangci-lint", "native:sqlc"} <= targets.keys(), "Missing native security coverage")
    return targets


def go_scan(policy, imported, go_version):
    results = {}
    for scope in policy["source_scopes"]:
        name = "root" if scope == "." else scope.replace("/", "-")
        for test in (False, True):
            suffix = "tests" if test else "production"
            args = [SCANNERS / "govulncheck", "-db=https://vuln.go.dev", "-format=json", "-scan=symbol"]
            text = run([*args, *(["-test"] if test else []), "./..."], REPO / scope, name + "-" + suffix)
            results[f"{scope}:{suffix}"] = govuln_result(text, policy, scope, imported, go_version)
    # Analyze the pinned scanner command sources including tests. Binary mode
    # fabricates OpenPGP wildcard symbols for Gitleaks despite absent packages,
    # as also observed for Buf in docs/validation/native-tool-security.md.
    scope = "tooling/security/scanners"
    text = run([SCANNERS / "govulncheck", "-db=https://vuln.go.dev", "-format=json",
                "-scan=symbol", "-test", *[package for package, _ in TOOLS.values()]],
               REPO / scope, "scanner-sources")
    results["scanner:sources"] = govuln_result(text, policy, scope, imported, go_version)
    for identity, binary in binary_targets(policy).items():
        text = run([SCANNERS / "govulncheck", "-db=https://vuln.go.dev", "-format=json",
                    "-scan=symbol", "-mode=binary", binary], REPO, identity.replace(":", "-"))
        # Native binaries get no exception: any finding requires review/update.
        results[identity] = govuln_result(text, policy, identity, {}, go_version, "binary")
    return results


def npm_scan(paths, scratch):
    results = {}
    for path in paths:
        if Path(path).name != "package-lock.json":
            continue
        directory = scratch / "npm" / Path(path).parent
        directory.mkdir(parents=True, exist_ok=True)
        for name in ("package.json", "package-lock.json"):
            shutil.copyfile(REPO / Path(path).parent / name, directory / name)
        # No node_modules, install, lifecycle scripts, omitted dev deps, or personal npmrc.
        code, stdout, stderr = command(["npm", "audit", "--package-lock-only", "--include=dev",
                                       "--include=optional", "--audit-level=high", "--json",
                                       "--registry=https://registry.npmjs.org"], directory)
        name = str(Path(path).parent).replace("/", "-")
        (RAW / (name + "-npm.json")).write_text(stdout)
        (RAW / (name + "-npm.stderr")).write_text(stderr)
        results[str(Path(path).parent)] = npm_result(json.loads(stdout), code)
        require((directory / "package-lock.json").read_bytes() == (REPO / path).read_bytes(), "npm changed lockfile")
    require("tooling/rpc" in results, "Missing required npm lockfile audit")
    return results


def workflow_scan(paths):
    workflows = [REPO / p for p in paths if p.startswith(".github/workflows/")
                 and Path(p).suffix in (".yml", ".yaml")]
    require(workflows, "Missing CI workflow")
    run([SCANNERS / "actionlint", "-shellcheck=", "-pyflakes=", *workflows], REPO, "actionlint")
    return {"files": len(workflows)}


def main():
    os.umask(0o077)
    OUTPUT.mkdir(parents=True, exist_ok=True)
    RAW.mkdir(mode=0o700, exist_ok=True)
    summary = {"schema": 1, "status": "failed", "started": datetime.now(timezone.utc).isoformat(), "checks": {}}
    write_json(OUTPUT / "summary.json", summary)
    stage = "initialization"
    try:
        policy = load_policy(REPO / "tooling/security/policy.json")
        verify(REPO, shutil.which("go"), SCANNERS)
        paths = source_paths()
        known = set(policy["graph_scopes"] + policy["native_scopes"])
        actual = {str(Path(p).parent) for p in paths if Path(p).name == "go.mod"}
        require(actual == known, "Go module coverage changed; review security scope")
        go_version = run(["go", "env", "GOVERSION"], REPO, "compiler").strip()
        with tempfile.TemporaryDirectory(prefix="scan-", dir=OUTPUT) as tmp:
            scratch = Path(tmp)
            # Npm personal/global config and script hooks cannot alter audit scope.
            user_config, global_config = scratch / "user.npmrc", scratch / "global.npmrc"
            user_config.write_text("")
            global_config.write_text("")
            for key in list(os.environ):
                if key.lower().startswith("npm_config_"):
                    del os.environ[key]
            os.environ.update({"NPM_CONFIG_USERCONFIG": str(user_config), "NPM_CONFIG_GLOBALCONFIG": str(global_config),
                               "NPM_CONFIG_IGNORE_SCRIPTS": "true", "NPM_CONFIG_CACHE": str(OUTPUT / "npm-cache")})
            snapshot = scratch / "source"
            snapshot.mkdir()
            before = materialize(paths, snapshot)
            write_json(RAW / "source-inventory.json", before)
            for stage, scan in [
                ("secrets", lambda: secrets(policy, snapshot, scratch)),
                ("workflow", lambda: workflow_scan(paths)),
            ]:
                print(f"security: {stage}", flush=True)
                summary["checks"][stage] = scan()
            stage = "go-inventory"
            print(f"security: {stage}", flush=True)
            imported, pairs = inventories(policy)
            for stage, scan in [("go-graphs", lambda: graph_scan(policy, imported, pairs, go_version)),
                                ("govulncheck", lambda: go_scan(policy, imported, go_version)),
                                ("npm", lambda: npm_scan(paths, scratch))]:
                print(f"security: {stage}", flush=True)
                summary["checks"][stage] = scan()
            stage = "source-consistency"
            after = scratch / "after"
            after.mkdir()
            require(paths == source_paths() and before == materialize(paths, after), "Source changed during security scan; rerun")
        summary["status"] = "passed"
    except Exception as error:
        # Do not print arbitrary exception strings: scanner/network failures can contain payloads.
        summary["failed_check"] = stage
        reason = str(error) if isinstance(error, PolicyError) else type(error).__name__
        summary["reason"] = reason
        (RAW / "failure.txt").write_text(reason)
        print(f"security: FAILED ({stage}): {reason}", file=sys.stderr)
    finally:
        summary["finished"] = datetime.now(timezone.utc).isoformat()
        write_json(OUTPUT / "summary.json", summary)
    if summary["status"] != "passed":
        return 1
    print("security: passed; sanitized summary: .artifacts/security/summary.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
