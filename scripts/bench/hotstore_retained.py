"""Opt-in retained-file measurement using freshly generated synthetic data."""
from contextlib import ExitStack
import json

from scripts.lib import benchmark as bench


def validate(root, records, message_bytes):
    assert root.is_dir() and not root.is_symlink(), 'invalid retained evidence directory'
    assert {path.name for path in root.iterdir()} == {'report.json', 'open.json', 'inventory.json'}
    for path in root.iterdir():
        assert path.is_file() and not path.is_symlink(), 'invalid retained evidence file'
        assert 0 < path.stat().st_size <= 128 << 20, 'retained report exceeds budget'
        bench.public_values(json.loads(path.read_text()))
    report = json.loads((root / 'report.json').read_text())
    assert report['passed'] and report['fixture_removed']
    assert report['records'] == records and report['message_bytes'] == message_bytes
    assert set(report['phases']) == {'open', 'inventory'}
    for name, phase in report['phases'].items():
        assert phase['phase'] == name
        assert phase['verified_bodies'] == records and phase['verified_locators'] == records
        assert phase['records'] == records and phase['message_bytes'] == message_bytes
        assert phase['elapsed_ms'] > 0
        assert phase['encoded_file_bytes'] == report['encoded_file_bytes']
    return report


def main(args):
    if len(args) not in (1, 3):
        raise bench.UsageError('Usage: scripts/bench.sh hotstore-retained NEW_OUTPUT_DIRECTORY [RECORDS MESSAGE_BYTES]')
    output = bench.new_output(args[0])
    records, message_bytes = args[1:] if len(args) == 3 else ('1024', '1024')
    if (records, message_bytes) not in {('1024', '1024'), ('16384', '1024'), ('65536', '1024'), ('1024', '65536')}:
        raise bench.UsageError('Only fixed workloads 1024/16384/65536 x 1024 or 1024 x 65536 are supported.')
    with ExitStack() as evidence:
        with bench.Run('hotstore-retained', failure_directory=output) as run:
            fixtures = run.root / 'fixtures'
            fixtures.mkdir(mode=0o700)
            results = run.root / 'results'
            environment = run.environment(
                FRY_HOTSTORE_RETAINED_MEASUREMENT='1', FRY_HOTSTORE_RETAINED_RECORDS=records,
                FRY_HOTSTORE_RETAINED_BYTES=message_bytes, FRY_HOTSTORE_RETAINED_OUTPUT_DIR=str(results))
            environment['TMPDIR'] = str(fixtures)
            for suffix in ('PHASE', 'ROOT', 'CHILD_REPORT', 'FILE_ID', 'FILE_BYTES'):
                environment.pop('FRY_HOTSTORE_RETAINED_' + suffix, None)
            print(f'Measuring fresh hotstore: {records} records x {message_bytes} bytes; no PostgreSQL or production data.', flush=True)
            run.run('go', 'test', '-count=1', '-timeout=15m', '-run', '^TestHotstoreRetainedMeasurement$',
                    '-v', './internal/hotstore', timeout=960, env=environment, log='test.log')
            report = validate(results, int(records), int(message_bytes))
            (results / 'test.log').write_text(bench.diagnostic(run.log))
            stage = evidence.enter_context(bench.evidence_directory(results, output.parent))
        bench.publish_directory(stage, output)
    print(json.dumps({'passed': True, 'profile': report['profile'], 'records': report['records'],
                      'message_bytes': report['message_bytes'], 'report': str(output / 'report.json')}, sort_keys=True))


if __name__ == '__main__':
    raise SystemExit(bench.entrypoint(main))
