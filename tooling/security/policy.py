"""Fail-closed parsers. Never include scanner payloads or secret values in errors."""

from datetime import date
import hashlib
import json


class PolicyError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise PolicyError(message)


def json_stream(text):
    decoder = json.JSONDecoder()
    values = []
    while text.strip():
        text = text.lstrip()
        try:
            value, end = decoder.raw_decode(text)
        except ValueError:
            raise PolicyError("Invalid scanner JSON") from None
        require(isinstance(value, dict), "Invalid scanner message")
        values.append(value)
        text = text[end:]
    require(values, "Empty scanner output")
    return values


def load_policy(path, today=None):
    policy = json.loads(path.read_text())
    today = today or date.today()
    require(policy["schema"] == 1, "Unsupported security policy")
    for entry in [*policy["secret_exceptions"], *policy["go_exceptions"]]:
        require(bool(entry["reason"]) and bool(entry["evidence"]), "Exception lacks rationale/evidence")
        require(date.fromisoformat(entry["reviewed"]) <= today < date.fromisoformat(entry["expires"]),
                "Security exception expired or review is in the future")
    return policy


def go_allowed(policy, scope, advisory, module, version, imported, trace=()):
    # Graph-only acceptance always requires an independently loaded test inventory.
    return any(advisory == e["advisory"] and module == e["module"] and version == e["version"]
               and scope in e["scopes"] and scope in imported
               and not any(p == e["package"] or p.startswith(e["package"] + "/") for p in imported[scope])
               and not any(frame.get("package") or frame.get("function") for frame in trace)
               for e in policy["go_exceptions"])


def govuln_result(text, policy, scope, imported, go_version, mode="source"):
    messages = json_stream(text)
    require(all(len(m) == 1 and set(m) <= {"config", "SBOM", "progress", "osv", "finding"}
                for m in messages), "Unexpected govulncheck message")
    configs = [m["config"] for m in messages if "config" in m]
    sboms = [m["SBOM"] for m in messages if "SBOM" in m]
    require(len(configs) == len(sboms) == 1, "Incomplete govulncheck scan")
    config = configs[0]
    require(config.get("scanner_name") == "govulncheck" and config.get("scanner_version") == "v1.9.0"
            and config.get("protocol_version") == "v1.0.0"
            and config.get("db") == "https://vuln.go.dev" and bool(config.get("db_last_modified"))
            and config.get("scan_level") == "symbol" and config.get("scan_mode") == mode,
            "Unexpected govulncheck configuration")
    require(sboms[0].get("go_version") == go_version and bool(sboms[0].get("modules")),
            "Missing or mismatched govulncheck inventory")
    osvs = {m["osv"]["id"] for m in messages if "osv" in m}
    findings = [m["finding"] for m in messages if "finding" in m]
    for finding in findings:
        trace = finding.get("trace", [])
        require(trace and finding.get("osv") in osvs, "Incomplete Go finding")
        module, version = trace[0].get("module"), trace[0].get("version")
        require(module and version, "Incomplete Go finding identity")
        require(go_allowed(policy, scope, finding["osv"], module, version, imported, trace),
                "Unaccepted Go advisory; inspect local report")
    return {"findings": len(findings), "accepted": len(findings), "database_modified": config["db_last_modified"]}


def npm_result(report, returncode):
    require(returncode in (0, 1) and isinstance(report, dict) and "error" not in report,
            "npm audit failed")
    require(report.get("auditReportVersion") == 2 and isinstance(report.get("vulnerabilities"), dict),
            "Incomplete npm audit report")
    counts = report.get("metadata", {}).get("vulnerabilities", {})
    severities = ("info", "low", "moderate", "high", "critical")
    require(all(type(counts.get(s)) is int and counts[s] >= 0 for s in severities),
            "Incomplete npm audit counts")
    actual = {s: 0 for s in severities}
    for value in report["vulnerabilities"].values():
        require(isinstance(value, dict) and value.get("severity") in severities,
                "Invalid npm audit finding")
        actual[value["severity"]] += 1
    require(actual == {s: counts[s] for s in severities}, "Inconsistent npm audit findings")
    require(counts.get("total") == sum(actual.values()), "Inconsistent npm audit total")
    require(not (counts["high"] or counts["critical"]), "High or critical npm advisory")
    return {s: counts[s] for s in severities}


def secret_result(report, returncode, policy):
    require(returncode in (0, 23) and isinstance(report, list), "Secret scanner failed")
    require((returncode == 23) == bool(report), "Inconsistent secret scanner result")
    sanitized = []
    for finding in report:
        require(isinstance(finding, dict) and isinstance(finding.get("Secret"), str)
                and isinstance(finding.get("File"), str) and isinstance(finding.get("RuleID"), str),
                "Invalid secret finding")
        fingerprint = hashlib.sha256(finding["Secret"].encode()).hexdigest()
        accepted = any(finding["RuleID"] == e["rule"] and finding["File"] == e["path"]
                       and fingerprint == e["sha256"] for e in policy["secret_exceptions"])
        sanitized.append({"path": finding["File"], "rule": finding["RuleID"],
                          "line": finding.get("StartLine"), "accepted": accepted})
    # Caller can retain sanitized locations locally, even when rejecting new findings.
    return sanitized


def osv_result(response, pairs):
    require(isinstance(response, dict) and "results" in response
            and isinstance(response["results"], list) and len(response["results"]) == len(pairs),
            "Incomplete OSV response")
    findings = []
    for pair, result in zip(pairs, response["results"]):
        require(isinstance(result, dict) and set(result) <= {"vulns", "next_page_token"}
                and not result.get("next_page_token"), "Incomplete or paginated OSV response")
        require(isinstance(result.get("vulns", []), list), "Invalid OSV findings")
        for vuln in result.get("vulns", []):
            require(isinstance(vuln, dict) and isinstance(vuln.get("id"), str)
                    and bool(vuln["id"]), "Invalid OSV finding")
            findings.append((*pair, vuln["id"]))
    return findings
