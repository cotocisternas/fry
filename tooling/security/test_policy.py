"""Policy boundaries and real secret-scanner regressions; no credentials in fixtures."""

from datetime import date
import hashlib
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch

import check
from policy import (PolicyError, go_allowed, govuln_result, json_stream,
                    load_policy, npm_result, osv_result, secret_result)


ROOT = Path(__file__).resolve().parents[2]


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.policy = load_policy(ROOT / "tooling/security/policy.json", date(2026, 10, 18))
        self.scope = "tooling/testdeps"
        self.imported = {self.scope: {"golang.org/x/crypto/ssh"}}

    def stream(self, finding=None, mode="source"):
        messages = [{"config": {"scanner_name": "govulncheck", "scanner_version": "v1.9.0",
                                "protocol_version": "v1.0.0", "db": "https://vuln.go.dev",
                                "db_last_modified": "2026-10-08T22:31:09Z", "scan_level": "symbol", "scan_mode": mode}},
                    {"SBOM": {"go_version": "go1.27.2", "modules": [{"path": "stdlib", "version": "v1.27.2"}]}}]
        if finding:
            messages += [{"osv": {"id": finding["osv"]}}, {"finding": finding}]
        return "\n".join(json.dumps(m) for m in messages)

    def finding(self):
        return {"osv": "GO-2026-5932", "trace": [{"module": "golang.org/x/crypto", "version": "v0.58.0"}]}

    def govuln(self, text):
        return govuln_result(text, self.policy, self.scope, self.imported, "go1.27.2")

    def test_empty_truncated_unknown_and_incomplete_streams_fail(self):
        for text in ("", "{} trailing", '{"config":{}}', '{"error":"unavailable"}'):
            with self.subTest(text=text), self.assertRaises(PolicyError):
                self.govuln(text)
        with self.assertRaises(PolicyError):
            json_stream('[]')

    def test_zero_exit_json_does_not_hide_new_go_findings(self):
        finding = self.finding()
        finding["osv"] = "GO-2099-1000"
        with self.assertRaises(PolicyError):
            self.govuln(self.stream(finding))

    def test_go_exception_requires_exact_version_scope_and_import_proof(self):
        self.assertEqual(1, self.govuln(self.stream(self.finding()))["accepted"])
        for scope, version, imported in [(".", "v0.58.0", {}),
                                         (self.scope, "v0.57.0", self.imported),
                                         ("third_party/go-smtp", "v0.58.0", {"third_party/go-smtp": set()})]:
            self.assertFalse(go_allowed(self.policy, scope, "GO-2026-5932", "golang.org/x/crypto", version, imported))

    def test_imported_openpgp_or_package_symbol_trace_always_fails(self):
        for package in ("golang.org/x/crypto/openpgp", "golang.org/x/crypto/openpgp/packet"):
            self.imported[self.scope].add(package)
            with self.assertRaises(PolicyError):
                self.govuln(self.stream(self.finding()))
            self.imported[self.scope].remove(package)
        for key in ("package", "function"):
            finding = self.finding()
            finding["trace"][0][key] = "affected"
            with self.assertRaises(PolicyError):
                self.govuln(self.stream(finding))

    def test_database_and_binary_inventory_must_match(self):
        for text in (self.stream().replace("https://vuln.go.dev", "file:///offline"),
                     self.stream().replace("go1.27.2", "go1.27.0"),
                     self.stream().replace('"modules": [{"path": "stdlib", "version": "v1.27.2"}]', '"modules": []')):
            with self.assertRaises(PolicyError):
                self.govuln(text)

    def test_expiration_is_enforced_on_deadline(self):
        with self.assertRaises(PolicyError):
            load_policy(ROOT / "tooling/security/policy.json", date(2026, 11, 18))

    def test_osv_rejects_network_error_partial_pagination_and_unknown_findings(self):
        pairs = [("module", "v1.0.0")]
        for report in ({}, {"error": "offline"}, {"results": []},
                       {"results": [{"next_page_token": "more"}]}, {"results": [{"vulns": [{}]}]}):
            with self.assertRaises(PolicyError):
                osv_result(report, pairs)
        self.assertEqual([], osv_result({"results": [{}]}, pairs))
        self.assertEqual([("module", "v1.0.0", "NEW")],
                         osv_result({"results": [{"vulns": [{"id": "NEW"}]}]}, pairs))

    def audit(self, severity=None):
        counts = {s: 0 for s in ("info", "low", "moderate", "high", "critical")}
        findings = {}
        if severity:
            counts[severity] = 1
            findings["package"] = {"severity": severity}
        return {"auditReportVersion": 2, "vulnerabilities": findings,
                "metadata": {"vulnerabilities": {**counts, "total": int(bool(severity))}}}

    def test_npm_high_critical_errors_and_inconsistent_results_fail(self):
        for report, code in ((self.audit("high"), 1), (self.audit("critical"), 1), ({"error": {}}, 0),
                             (self.audit(), 2), ({}, 0)):
            with self.assertRaises(PolicyError):
                npm_result(report, code)
        inconsistent = self.audit("high")
        inconsistent["metadata"]["vulnerabilities"]["high"] = 0
        with self.assertRaises(PolicyError):
            npm_result(inconsistent, 0)
        self.assertEqual(1, npm_result(self.audit("moderate"), 0)["moderate"])

    def test_secret_exception_binds_bytes_path_and_rule_and_sanitizes(self):
        secret = "synthetic-policy-value"
        policy = {"secret_exceptions": [{"path": ".hidden/fixture", "rule": "private-key",
                                        "sha256": hashlib.sha256(secret.encode()).hexdigest()}]}
        finding = {"Secret": secret, "File": ".hidden/fixture", "RuleID": "private-key", "StartLine": 1}
        result = secret_result([finding], 23, policy)
        self.assertTrue(result[0]["accepted"])
        self.assertNotIn(secret, json.dumps(result))
        for key in ("Secret", "File", "RuleID"):
            changed = {**finding, key: finding[key] + "changed"}
            self.assertFalse(secret_result([changed], 23, policy)[0]["accepted"])
        for report, code in (([], 23), ([finding], 0), ([], 1)):
            with self.assertRaises(PolicyError):
                secret_result(report, code, policy)

    def test_all_native_binaries_are_actually_scanned(self):
        policy = {**self.policy, "source_scopes": []}
        with patch.object(check, "run", return_value=self.stream(mode="binary")) as run, \
                patch.object(check, "govuln_result", return_value={}):
            results = check.go_scan(policy, {}, "go1.27.2")
        for name in ("golangci-lint", "sqlc"):
            self.assertIn("native:" + name, results)
            self.assertTrue(any(Path(call.args[0][-1]).name == name and "-mode=binary" in call.args[0]
                                for call in run.call_args_list))
        self.assertTrue(any("-test" in call.args[0] and check.TOOLS["gitleaks"][0] in call.args[0]
                            for call in run.call_args_list))
        with self.assertRaises(PolicyError):
            check.binary_targets({**policy, "native_scopes": []})


class LiveSecretTests(unittest.TestCase):
    def test_hidden_inline_allowed_and_deleted_history_secrets_are_detected(self):
        policy = load_policy(ROOT / "tooling/security/policy.json", date(2026, 10, 18))
        key = re.search(rb"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----",
                        (ROOT / policy["secret_exceptions"][0]["path"]).read_bytes(), re.S).group()
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            def command(args):
                code, stdout, _ = check.command(args, repo, timeout=30)
                self.assertEqual(0, code)
                return stdout
            command(["git", "init", "-q"])
            hidden = repo / ".hidden"
            hidden.mkdir()
            fixture = hidden / "test.pem"
            fixture.write_bytes(key + b"\n# gitleaks:allow\n")
            args = [check.SCANNERS / "gitleaks", "dir", ".", "--config", ROOT / "tooling/security/gitleaks.toml",
                    "--ignore-gitleaks-allow", "--gitleaks-ignore-path", repo / "missing.ignore",
                    "--report-format=json", "--report-path=-", "--no-banner", "--log-level=error", "--exit-code=23"]
            code, stdout, stderr = check.command(args, repo, timeout=30)
            self.assertEqual("", stderr)
            findings = secret_result(json.loads(stdout), code, policy)
            self.assertTrue(findings)
            self.assertTrue(all(not f["accepted"] for f in findings))
            command(["git", "add", ".hidden/test.pem"])
            author = ["git", "-c", "user.name=security-policy-test", "-c", "user.email=security-policy-test@invalid.local"]
            command([*author, "commit", "-q", "-m", "synthetic public key fixture"])
            command(["git", "rm", "-q", ".hidden/test.pem"])
            command([*author, "commit", "-q", "-m", "delete fixture"])
            args[1] = "git"
            args.append("--log-opts=--all --full-history")
            code, stdout, stderr = check.command(args, repo, timeout=30)
            self.assertEqual("", stderr)
            findings = secret_result(json.loads(stdout), code, policy)
            self.assertTrue(findings)
            self.assertTrue(all(not f["accepted"] for f in findings))


if __name__ == "__main__":
    unittest.main()
