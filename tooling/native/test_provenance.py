"""Security-boundary checks without downloads or real tool installation."""

import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("provenance", Path(__file__).with_name("provenance.py"))
provenance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(provenance)


class ProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.manifest = self.repo / "tooling/native/sqlc/go.mod"
        self.manifest.parent.mkdir(parents=True)
        self.manifest.write_text("tool example.com/sqlc/cmd/sqlc\nrequire (\n"
                                 " example.com/sqlc v1.0.0\n example.com/dep v2.0.0\n)\n")
        self.manifest.with_suffix(".sum").write_text("example.com/sqlc v1.0.0 h1:source\n"
                                                   "example.com/dep v2.0.0 h1:dep\n")
        for name in ("mise.toml", "mise.lock", "tooling/versions.env", "scripts/setup.sh",
                     "scripts/toolchain.sh", "tooling/native/build.sh", "tooling/native/provenance.py"):
            path = self.repo / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("reviewed input\n")
        self.binary = self.repo / "sqlc"
        self.binary.write_bytes(b"compiled tool")
        self.info = {
            "GoVersion": "go1.27.2", "Path": "example.com/sqlc/cmd/sqlc",
            "Main": {"Path": "example.com/sqlc", "Version": "v1.0.0", "Sum": "h1:source"},
            "Deps": [{"Path": "example.com/dep", "Version": "v2.0.0", "Sum": "h1:dep"}],
            "Settings": [{"Key": key, "Value": value} for key, value in {
                "-trimpath": "true", "-buildmode": "exe", "-compiler": "gc", "CGO_ENABLED": "0",
                "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1"}.items()]}

    def snapshot(self, info=None):
        payload = self.info if info is None else info
        with patch.object(provenance.subprocess, "check_output",
                          side_effect=[json.dumps(payload), "amd64\n"]):
            return provenance.snapshot(self.repo, "sqlc", "go", self.binary, "1.27.2", "1")

    def test_identical_inputs_are_stable(self):
        self.assertEqual(self.snapshot(), self.snapshot())

    def test_changed_binary_and_build_recipe_invalidate_receipt(self):
        original = self.snapshot()
        self.binary.write_bytes(b"altered executable with the same version")
        self.assertNotEqual(original, self.snapshot())
        self.binary.write_bytes(b"compiled tool")
        (self.repo / "tooling/native/build.sh").write_text("changed recipe\n")
        self.assertNotEqual(original, self.snapshot())

    def test_old_compiler_is_rejected(self):
        info = copy.deepcopy(self.info)
        info["GoVersion"] = "go1.26.2"
        with self.assertRaisesRegex(ValueError, "Compiler"):
            self.snapshot(info)

    def test_wrong_release_or_command_is_rejected(self):
        for field in ("Main", "Path"):
            info = copy.deepcopy(self.info)
            if field == "Main":
                info[field]["Version"] = "v0.9.0"
            else:
                info[field] = "example.com/other/cmd/sqlc"
            with self.assertRaises(ValueError):
                self.snapshot(info)

    def test_old_dependency_unverified_sum_and_replacement_are_rejected(self):
        for mutation in ({"Version": "v1.0.0"}, {"Sum": "h1:other"},
                         {"Replace": {"Path": "local/patch"}}):
            info = copy.deepcopy(self.info)
            info["Deps"][0].update(mutation)
            with self.assertRaisesRegex(ValueError, "Unpinned"):
                self.snapshot(info)

    def test_wrong_platform_or_cgo_is_rejected(self):
        for key, value in (("CGO_ENABLED", "1"), ("GOARCH", "arm64"), ("GOAMD64", "v3")):
            info = copy.deepcopy(self.info)
            for setting in info["Settings"]:
                if setting["Key"] == key:
                    setting["Value"] = value
            with self.assertRaises(ValueError):
                self.snapshot(info)

    def test_linter_dependency_upgrades_are_rejected(self):
        manifest = self.repo / "tooling/native/golangci-lint/go.mod"
        manifest.parent.mkdir(parents=True)
        upstream = self.repo / "upstream.mod"
        # Multiline require blocks are the normal upstream graph format.
        manifest.write_text("require (\n example.com/linter v2.0.0\n)\n")
        upstream.write_text("require (\n example.com/linter v1.0.0\n)\n")
        with self.assertRaisesRegex(ValueError, "differs from upstream"):
            provenance.lint_graph(self.repo, upstream)


if __name__ == "__main__":
    unittest.main()
