"""Real accepted local originals; fixed mailbox drain measurement only."""
import json
from pathlib import Path

from scripts.lib import benchmark as bench


def validate(text):
    marker = 'PUBLICATION_MEASUREMENT_JSON '
    cases = [json.loads(line.split(marker, 1)[1]) for line in text.splitlines() if marker in line]
    assert [value['limit'] for value in cases] == [1, 16, 64, 128]
    assert all(value['messages'] == 1024 and value['verified_originals'] == 1024 for value in cases)
    report = {'profile': 'bounded-local-publication-baseline', 'cases': cases,
              'wal_scope': 'whole isolated PostgreSQL cluster, background noise possible',
              'measurement_scope': 'production source identified by captured hashes; transaction counts are not instrumented'}
    bench.public_values(report)
    return report


def main(args):
    if len(args) > 1:
        raise bench.UsageError('Usage: scripts/bench.sh mailbox-publication [NEW_REPORT_JSON]')
    if args:
        requested = Path(args[0])
        if requested.exists() or requested.is_symlink():
            raise bench.UsageError('Report output must be a new file')
    output = bench.new_output(args[0]) if args else None
    with bench.Run('publication-measure') as run:
        run.fixture()
        result = run.run('go', 'test', '-count=1', '-timeout=240s', '-run', '^TestMailboxPublicationMeasurement$',
                         '-v', './internal/receiptstore', timeout=300,
                         env=run.environment(FRY_PUBLICATION_MEASUREMENT='1'), log='result.log')
        report = validate(result.stdout.decode())
        bench.public_values(report, run.secrets)
    if output:
        bench.private_json(output, report)
    print(json.dumps(report, sort_keys=True))


if __name__ == '__main__':
    raise SystemExit(bench.entrypoint(main))
