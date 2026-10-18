"""Reproducible illustrative FRY pack pricing, not sizing or a complete AWS bill.

Run this file to check arithmetic and regenerate packed-storage-cost-model.json.
Import estimate(**overrides) for individual scenarios. The earlier model is read,
never modified; its unchanged estimate is returned for a like-workload comparison.
All fractions are 0..1 except rewrite/traffic multipliers, which may exceed 1.
"""
import importlib.util
import json
import math
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location(
    "individual_object_baseline", HERE / "sao-paulo-cost-model.py")
baseline = importlib.util.module_from_spec(spec)
spec.loader.exec_module(baseline)
RATES = dict(baseline.RATES, cross_az_endpoint_gb=.01)
DEFAULTS = {
    "million_day": 20, "mean_kb": 250, "storage_days": 30,
    "external_percent": 50, "pack_mib": 64, "fill_ratio": .8,
    "compaction_rewrite_fraction": .1, "compaction_live_fraction": .8,
    "cleanup_hours": 24, "compaction_overlap_hours": 1,
    "normal_hot_hours": 1, "outage_hours": 6, "hot_copies": 2,
    "hot_headroom": .3, "extra_storage_nodes": 3,
    "extra_cross_az_streams": 0, "read_gets_per_message": 1,
    "upload_retry_fraction": 0,
}
FORMULAS = {
    "monthly_original_bytes": "million_day * 1e6 * mean_kb * 1000 * 30",
    "hot_provisioned_bytes": "hourly_original_bytes * (outage_hours + normal_hot_hours) * hot_copies * (1 + hot_headroom); round each equally loaded worker up to GiB",
    "pack_payload_bytes": "pack_mib * 2**20 * fill_ratio; no padding charge for unused target capacity",
    "initial_puts": "ceil(monthly_original_bytes / pack_payload_bytes)",
    "rewrite_bytes": "monthly_original_bytes * compaction_rewrite_fraction",
    "compaction_read_bytes": "rewrite_bytes / compaction_live_fraction",
    "rewrite_puts": "ceil(rewrite_bytes / pack_payload_bytes)",
    "compaction_gets": "ceil(compaction_read_bytes / pack_payload_bytes); full-pack reads",
    "verification_heads": "one HEAD per successful initial/rewrite PUT, charged at Tier2 GET rate",
    "retirement_requests": "one conditional zero-byte retirement PUT and one verification HEAD per pack; steady-state retired packs approximated by initial_puts + rewrite_puts",
    "retry_puts": "ceil((initial_puts + rewrite_puts) * upload_retry_fraction)",
    "dead_bytes_illustration": "hourly_original_bytes * cleanup_hours; assumes steady expiry at incoming byte rate and every dead byte stays for the whole deadline",
    "rewrite_overlap_bytes": "rewrite_bytes / (30 * 24) * compaction_overlap_hours",
    "s3_average_bytes": "daily_original_bytes * storage_days + dead_bytes_illustration + rewrite_overlap_bytes; no storage credit for hot residency",
    "cross_az_replica": "monthly_original_bytes / 1e9 * (hot_copies - 1) * 2 * .01",
    "extra_cross_az": "monthly_original_bytes / 1e9 * extra_cross_az_streams * 2 * .01",
    "internet_and_nlb": "unchanged earlier baseline for the same incoming workload and external_percent",
}


def estimate(**overrides):
    """Return assumptions, volumes, operations, components, baseline, and delta.

    Parameters and defaults are exported as DEFAULTS. Monetary components are
    unrounded USD; volumes retain explicit byte/GiB/decimal-GB units. Fractions
    describe scenario assumptions, not measured FRY behavior.
    """
    unknown = set(overrides) - DEFAULTS.keys()
    if unknown:
        raise ValueError(f"Unknown assumptions: {sorted(unknown)}")
    a = dict(DEFAULTS, **overrides)
    for k, v in a.items():
        if not isinstance(v, (int, float)) or not math.isfinite(v) or v < 0:
            raise ValueError(f"{k} must be finite and nonnegative")
    for k in ("fill_ratio", "compaction_live_fraction"):
        if not 0 < a[k] <= 1:
            raise ValueError(f"{k} must be in (0,1]")
    if a["external_percent"] > 100 or a["pack_mib"] <= 0:
        raise ValueError("external_percent must be <=100 and pack_mib positive")
    for k in ("hot_copies", "extra_storage_nodes"):
        if a[k] < 1 or int(a[k]) != a[k]:
            raise ValueError(f"{k} must be a positive integer")
    r = RATES
    old = baseline.estimate(**{k: a[k] for k in
                              ("million_day", "mean_kb", "storage_days", "external_percent")})
    monthly_messages = a["million_day"] * 1e6 * r["traffic_days"]
    monthly_bytes = monthly_messages * a["mean_kb"] * 1000
    hourly_bytes = monthly_bytes / r["traffic_days"] / 24
    payload = a["pack_mib"] * 2**20 * a["fill_ratio"]
    rewrite_bytes = monthly_bytes * a["compaction_rewrite_fraction"]
    compaction_read_bytes = rewrite_bytes / a["compaction_live_fraction"]
    initial_puts = math.ceil(monthly_bytes / payload)
    rewrite_puts = math.ceil(rewrite_bytes / payload)
    successful_puts = initial_puts + rewrite_puts
    retry_puts = math.ceil(successful_puts * a["upload_retry_fraction"])
    verification_heads = successful_puts
    # Steady-state successful-pack creation ~= retirement, including compaction.
    # This is workload arithmetic, not a claim about exact calendar retirements.
    retirement_puts = successful_puts
    retirement_heads = retirement_puts
    consumer_gets = monthly_messages * a["read_gets_per_message"]
    compaction_gets = math.ceil(compaction_read_bytes / payload)
    logical_bytes = hourly_bytes * 24 * a["storage_days"]
    dead_bytes = hourly_bytes * a["cleanup_hours"]
    overlap_bytes = rewrite_bytes / r["traffic_days"] / 24 * a["compaction_overlap_hours"]
    physical_s3_bytes = logical_bytes + dead_bytes + overlap_bytes
    required_hot_bytes = hourly_bytes * (a["outage_hours"] + a["normal_hot_hours"]) * a["hot_copies"]
    hot_target_bytes = required_hot_bytes * (1 + a["hot_headroom"])
    per_worker_gib = math.ceil(hot_target_bytes / 2**30 / a["extra_storage_nodes"])
    hot_gib = per_worker_gib * a["extra_storage_nodes"]
    replica_streams = a["hot_copies"] - 1
    both_endpoint_gb_rate = 2 * r["cross_az_endpoint_gb"]
    components = {
        "S3 stored packs incl dead bytes and rewrite overlap": baseline.tiered(physical_s3_bytes / 2**30, r["s3_tiers"]),
        "S3 initial pack PUTs": initial_puts * r["put"],
        "S3 compaction PUTs": rewrite_puts * r["put"],
        "S3 retry PUTs": retry_puts * r["put"],
        "S3 consumer GETs": consumer_gets * r["get"],
        "S3 compaction GETs": compaction_gets * r["get"],
        "S3 verification HEADs": verification_heads * r["get"],
        "S3 zero-byte retirement PUTs": retirement_puts * r["put"],
        "S3 retirement verification HEADs": retirement_heads * r["get"],
        "Internet downloads": old["components"]["Internet downloads"],
        "NLB hour + byte dimension": old["components"]["NLB hour + byte dimension"],
        "Earlier six-worker compute + database + fixed network": old["components"]["Illustrative compute + database + fixed network"],
        "Additional storage worker compute": a["extra_storage_nodes"] * r["node_hour"] * r["hours"],
        "Additional storage worker root disks": a["extra_storage_nodes"] * r["node_disk_gib"] * r["ebs_gib_month"],
        "Hot replicated gp3 capacity": hot_gib * r["ebs_gib_month"],
        "Cross-AZ hot replication": monthly_bytes / 1e9 * replica_streams * both_endpoint_gb_rate,
        "Additional cross-AZ paths": monthly_bytes / 1e9 * a["extra_cross_az_streams"] * both_endpoint_gb_rate,
    }
    total = sum(components.values())
    return {
        "assumptions": a,
        "volumes": {
            "monthly_messages": monthly_messages,
            "monthly_original_bytes": monthly_bytes,
            "daily_original_decimal_tb": hourly_bytes * 24 / 1e12,
            "external_download_decimal_tb": old["external_decimal_tb"],
            "effective_pack_payload_bytes": payload,
            "original_bytes_per_pack_messages_equivalent": payload / (a["mean_kb"] * 1000) if a["mean_kb"] else None,
            "logical_stored_bytes": logical_bytes, "dead_bytes_illustration": dead_bytes,
            "rewrite_overlap_bytes": overlap_bytes, "s3_average_bytes": physical_s3_bytes,
            "hot_required_before_headroom_bytes": required_hot_bytes,
            "hot_target_before_rounding_bytes": hot_target_bytes,
            "hot_provisioned_gib": hot_gib, "hot_gib_per_storage_worker": per_worker_gib,
            "monthly_compaction_live_rewrite_bytes": rewrite_bytes,
            "monthly_compaction_full_pack_read_bytes": compaction_read_bytes,
            "monthly_s3_upload_bytes_including_rewrites_and_retries": (monthly_bytes + rewrite_bytes) * (1 + a["upload_retry_fraction"]),
            "monthly_cross_az_logical_bytes": monthly_bytes * (replica_streams + a["extra_cross_az_streams"]),
            "monthly_cross_az_billed_endpoint_decimal_gb": 2 * monthly_bytes * (replica_streams + a["extra_cross_az_streams"]) / 1e9,
        },
        "operations": {"initial_puts": initial_puts, "rewrite_puts": rewrite_puts,
                       "retry_puts": retry_puts, "consumer_gets": consumer_gets,
                       "compaction_gets": compaction_gets, "verification_heads": verification_heads,
                       "retirement_puts": retirement_puts, "retirement_heads": retirement_heads},
        "components": components, "subtotal_usd": total,
        "baseline_subtotal_usd": old["subtotal_usd"],
        "delta_usd": total - old["subtotal_usd"],
        "baseline": old,
    }


def self_test():
    """Check conservation, billing boundaries, and independent hand arithmetic."""
    original = json.loads((HERE / "sao-paulo-cost-model.json").read_text())
    for row in original["scenarios"]:
        current = baseline.estimate(mean_kb=row["mean_kb"])
        assert math.isclose(current["subtotal_usd"], row["subtotal_usd"], abs_tol=1e-8)
    base = estimate()
    assert base["volumes"]["monthly_original_bytes"] == 150_000_000_000_000
    assert base["volumes"]["external_download_decimal_tb"] == 75
    assert math.isclose(base["volumes"]["hot_target_before_rounding_bytes"], 5e12 / 24 * 7 * 2 * 1.3)
    assert base["volumes"]["hot_provisioned_gib"] * 2**30 >= base["volumes"]["hot_target_before_rounding_bytes"]
    assert base["volumes"]["dead_bytes_illustration"] == 5e12
    assert base["components"]["Cross-AZ hot replication"] == 3000
    assert base["components"]["S3 consumer GETs"] == 336
    assert base["operations"]["retirement_puts"] == base["operations"]["initial_puts"] + base["operations"]["rewrite_puts"]
    assert base["operations"]["retirement_heads"] == base["operations"]["retirement_puts"]
    for extra in (.5, 1):
        assert math.isclose(estimate(extra_cross_az_streams=extra)["subtotal_usd"] - base["subtotal_usd"], 3000 * extra)
    low_fill = estimate(fill_ratio=.2)
    assert low_fill["volumes"]["s3_average_bytes"] == base["volumes"]["s3_average_bytes"]
    assert abs(low_fill["operations"]["initial_puts"] - 4 * base["operations"]["initial_puts"]) <= 3
    no_rewrite = estimate(compaction_rewrite_fraction=0)
    assert no_rewrite["operations"]["rewrite_puts"] == 0
    assert no_rewrite["operations"]["compaction_gets"] == 0
    assert no_rewrite["volumes"]["rewrite_overlap_bytes"] == 0
    assert estimate(external_percent=0)["components"]["Internet downloads"] == 0
    assert estimate(million_day=0)["operations"]["initial_puts"] == 0
    assert estimate(million_day=0)["components"]["Cross-AZ hot replication"] == 0
    assert estimate(hot_copies=1)["components"]["Cross-AZ hot replication"] == 0
    for bad in ({"fill_ratio": 0}, {"external_percent": 101}, {"extra_storage_nodes": 0}, {"mean_kb": -1}):
        try:
            estimate(**bad)
        except ValueError:
            pass
        else:
            raise AssertionError(f"Invalid scenario accepted: {bad}")


def report():
    return {
        "checked": "2026-10-17", "region": "sa-east-1", "currency": "USD",
        "status": "illustrative partial cost model, not validated sizing or complete quote",
        "rates": RATES, "defaults": DEFAULTS, "formulas": FORMULAS,
        "sources": ["sao-paulo-compute-prices-2026-10-17.json",
                    "sao-paulo-storage-network-prices-2026-10-17.json",
                    "sao-paulo-database-prices-2026-10-17.json",
                    "https://aws.amazon.com/blogs/networking-and-content-delivery/optimizing-data-transfer-costs-when-using-aws-network-load-balancer/"],
        "scenarios": [estimate(mean_kb=k) for k in (100, 250, 500)],
        "fill_sensitivity": [estimate(fill_ratio=f) for f in (.2, .5, .8, 1)],
        "compaction_sensitivity": [estimate(compaction_rewrite_fraction=f) for f in (0, .1, .5, 1)],
        "network_sensitivity": [estimate(extra_cross_az_streams=f) for f in (0, .5, 1)],
        "headroom_sensitivity": [estimate(hot_headroom=f) for f in (.1, .3, .5)],
        "read_sensitivity": [estimate(read_gets_per_message=n) for n in (1, 2, 5)],
        "combined_stress": estimate(fill_ratio=.2, compaction_rewrite_fraction=1, extra_cross_az_streams=1),
    }


if __name__ == "__main__":
    self_test()
    data = report()
    Path(__file__).with_suffix(".json").write_text(json.dumps(data, indent=2) + "\n")
    print("Arithmetic and invariant checks passed.")
    for row in data["scenarios"]:
        print(f'{row["assumptions"]["mean_kb"]} KB: baseline ${row["baseline_subtotal_usd"]:,.2f}; '
              f'packed ${row["subtotal_usd"]:,.2f}; delta ${row["delta_usd"]:+,.2f}')
