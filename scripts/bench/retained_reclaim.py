"""Bounded first hot rewrite of real accepted synthetic local originals."""
import json

from scripts.lib import benchmark as bench


def validate(text, records, message_bytes):
    marker = 'RETAINED_RECLAIM_MEASUREMENT_JSON '
    cases = [json.loads(line.split(marker, 1)[1]) for line in text.splitlines() if marker in line]
    assert len(cases) == 1
    report = cases[0]
    assert report['records'] == records and report['message_bytes'] == message_bytes
    assert report['records'] == report['live_records'] + report['expired_records']
    assert report['live_records'] == report['verified_live_direct_from_rewritten_member'] == report['finished_removed_receipts']
    bench.public_values(report)
    return report


def main(args):
    if len(args) > 3:
        raise bench.UsageError('Usage: scripts/bench.sh retained-reclaim [RECORDS [MESSAGE_BYTES [NEW_REPORT_JSON]]]')
    records = int(args[0]) if args else 1024
    message_bytes = int(args[1]) if len(args) > 1 else 1024
    output = bench.new_output(args[2] if len(args) > 2 else '.fry-retained-reclaim-report.json')
    assert 128 <= records <= 4096 and records % 2 == 0 and 128 <= message_bytes <= 65536 and records * message_bytes <= 256 << 20, 'bounded count/size required'
    with bench.Run('retained-reclaim', failure_limit=16384) as run:
        run.fixture()
        environment = run.environment(FRY_RETAINED_RECLAIM_MEASUREMENT='1', FRY_RECLAIM_RECORDS=str(records),
                                      FRY_RECLAIM_MESSAGE_BYTES=str(message_bytes), GOFLAGS='')
        print(f'Running bounded real accepted hot rewrite: {records} records x {message_bytes} bytes', flush=True)
        result = run.run('go', 'test', '-count=1', '-timeout=300s', '-run', '^TestRetainedReclaimMeasurement$',
                         '-v', './internal/receiptstore', timeout=360, env=environment, log='result.log')
        report = validate(result.stdout.decode(), records, message_bytes)
        bench.public_values(report, run.secrets)
    report['fixture_cleanup_checked'] = True
    bench.private_json(output, report)
    print(json.dumps(report, sort_keys=True))


if __name__ == '__main__':
    raise SystemExit(bench.entrypoint(main))
