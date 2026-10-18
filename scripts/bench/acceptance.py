"""Fixed synthetic Store.Accept measurements and checked private artifacts."""
from contextlib import ExitStack
import gzip
import hashlib
import io
import json
from pathlib import Path
import stat

from scripts.lib import benchmark as bench
from scripts.lib import testdeps

ARTIFACT_LIMIT = 128 << 20


def validate(root, mode, storage_profile, max_message_bytes, secret):
    expected = {'report.json'}
    if mode == 'profile':
        expected |= {'cpu.pprof', 'block.pprof', 'mutex.pprof'}
    if mode == 'trace':
        expected.add('execution.trace')
    assert root.is_dir() and not root.is_symlink()
    assert {path.name for path in root.iterdir()} == expected, 'unexpected or missing measurement artifact'
    for path in root.iterdir():
        assert path.is_file() and not path.is_symlink()
        assert stat.S_IMODE(path.stat().st_mode) & 0o077 == 0, 'artifact permissions must be private'
        assert 0 < path.stat().st_size <= ARTIFACT_LIMIT, 'artifact exceeds128MiB budget'
        raw = path.read_bytes()
        inspected = gzip.GzipFile(fileobj=io.BytesIO(raw)).read(ARTIFACT_LIMIT + 1) if raw.startswith(b'\x1f\x8b') else raw
        assert len(inspected) <= ARTIFACT_LIMIT, 'expanded profile exceeds128MiB budget'
        assert secret.encode() not in inspected and b'fry-test-only' not in inspected, 'fixture credential leaked'
    report = json.loads((root / 'report.json').read_text())
    assert isinstance(report, dict), 'invalid measurement report'
    assert report.get('mode') == mode, 'measurement mode mismatch'
    assert report.get('storage_profile') == storage_profile
    assert report.get('member_capacity_bytes') == 256 << 20
    assert report.get('segment_max_records') == (2048 if storage_profile == 'bounded' else 0)
    assert report.get('segment_max_bytes') == (96 << 20 if storage_profile == 'bounded' else 0)
    if storage_profile == 'bounded':
        assert report.get('verified_capacity_files') == 2
        assert report.get('verified_capacity_targets') == 2048
        expected_lanes = 16 if max_message_bytes == 65536 else 11
        assert report.get('capacity_lane_version') == 1
        assert report.get('capacity_lanes_per_file') == expected_lanes
        assert report.get('verified_capacity_lanes') == 2 * expected_lanes
    assert report.get('sql_trace_enabled') is (mode != 'baseline')
    assert report.get('runtime_profiles_enabled') is (mode == 'profile')
    assert report.get('runtime_trace_enabled') is (mode == 'trace')
    for key, expected_value in {
            'messages': 1024, 'message_bytes': 65536, 'workers': 16, 'attempted': 1024,
            'succeeded': 1024, 'errors': 0, 'verified_receipts': 1024, 'verified_bodies': 1024,
            'max_concurrent_appends': 16, 'max_message_bytes': max_message_bytes,
            'lease_seconds': 30, 'sql_bucket_overflow': 0}.items():
        assert report.get(key) == expected_value, 'unexpected workload or incomplete verification: ' + key
    assert report.get('pool', {}).get('max_connections') == 16
    if mode == 'baseline':
        assert not report.get('sql') and report.get('acquire_trace', {}).get('count') == 0
        assert report.get('profile_block_rate_ns') == 0 and report.get('profile_mutex_fraction') == 0
    else:
        assert report.get('sql') and report.get('acquire_trace', {}).get('count', 0) > 0
    bench.public_values(report, (secret,), forbidden_keys=True)
    return report


def main(args):
    if not 2 <= len(args) <= 4:
        raise bench.UsageError('Usage: scripts/bench.sh acceptance baseline|profile|trace NEW_OUTPUT_DIRECTORY [legacy|bounded] [65536|1048576]')
    mode, destination = args[:2]
    storage_profile = args[2] if len(args) > 2 else 'legacy'
    max_bytes = args[3] if len(args) > 3 else '65536'
    if max_bytes not in ('65536', '1048576'):
        raise bench.UsageError('Unsupported maximum message size for fixed measurement')
    if storage_profile not in ('legacy', 'bounded'):
        raise bench.UsageError('Unknown storage profile')
    if mode not in ('baseline', 'profile', 'trace'):
        raise bench.UsageError('Unknown measurement mode')
    output = bench.new_output(destination)
    output.mkdir(mode=0o700)
    with ExitStack() as evidence:
        with bench.Run('acceptance', failure_directory=output) as run:
            database = run.fixture()
            image = testdeps.image()
            (run.root / 'artifacts').mkdir(mode=0o700)
            (run.root / 'temporary').mkdir(mode=0o700)
            artifact_root = run.root / 'artifacts' / mode
            environment = run.environment(
                FRY_ACCEPTANCE_MEASUREMENT=mode, FRY_ACCEPTANCE_STORAGE_PROFILE=storage_profile,
                FRY_ACCEPTANCE_MAX_MESSAGE_BYTES=max_bytes, FRY_ACCEPTANCE_OUTPUT_DIR=str(artifact_root),
                GOFLAGS='')
            environment['TMPDIR'] = str(run.root / 'temporary')
            print('Running fixed local acceptance measurement in ' + mode + ' mode.', flush=True)
            run.run('go', 'test', '-count=1', '-timeout=240s', '-run', '^TestAcceptanceMeasurement$',
                    '-v', './internal/receiptstore', timeout=300, env=environment, log='test.log')
            validate(artifact_root, mode, storage_profile, int(max_bytes), database.database_url)
            version = run.run('docker', 'exec', database.container, 'postgres', '--version', timeout=10).stdout.decode().strip()
            stage = evidence.enter_context(bench.evidence_directory(artifact_root, output))
        # Session close verifies container removal; scratch and test logs are gone.
        bench.publish_directory(stage, output / mode)
        artifacts = {path.name: {'bytes': path.stat().st_size,
                                'sha256': hashlib.sha256(path.read_bytes()).hexdigest()}
                     for path in sorted((output / mode).iterdir())}
        value = {
            'passed': True, 'synthetic_local_only': True, 'fixture_cleanup_checked': True,
            'mode': mode, 'storage_profile': storage_profile, 'max_message_bytes': int(max_bytes),
            'test': 'TestAcceptanceMeasurement', 'postgres_image': image, 'postgres_version': version,
            'runner_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            'test_timeout_seconds': 240, 'go_command_timeout_seconds': 300, 'artifacts': artifacts,
            'profiling_overhead': 'baseline disables runtime profiles/trace and SQL tracers; pool stats remain; profile and trace are separate diagnostic runs'}
        bench.private_json(output / 'run.json', value)
    print(json.dumps(value, sort_keys=True))


if __name__ == '__main__':
    raise SystemExit(bench.entrypoint(main))
