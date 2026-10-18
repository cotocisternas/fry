"""Disposable actual fry process and bounded numeric-loopback SMTP/IMAP load."""
import json
from pathlib import Path
import socket
import sys
import time

from scripts.lib import benchmark as bench
from scripts.lib import testdeps

PLACEMENT_SQL = "SELECT coalesce(json_agg(t ORDER BY shard),'[]'::json) FROM (SELECT s.id AS shard,count(a.id) AS accepted FROM fry_ingest_shard s LEFT JOIN fry_attempt a ON a.ingest_shard=s.id AND a.state='accepted' GROUP BY s.id) t"


def configuration(root, smtp, imap, count, size, workers, pairs):
    config = json.loads((bench.ROOT / 'examples/local-smtp.json').read_text())
    config.update(listen=f'127.0.0.1:{smtp}', data_directory=str(root / 'hot'), max_message_bytes=size,
                  member_capacity_bytes=(4 * count * size + 8 * workers * size + 67108864) // pairs,
                  max_connections=max(32, workers * 2 + 8), max_in_flight=workers)
    if pairs > 1:
        config.pop('members')
        config['ingest_shards'] = [
            {'id': f'{index + 101:032x}', 'members': [
                {'id': f'{index * 2 + 1:032x}', 'domain': 'local-a'},
                {'id': f'{index * 2 + 2:032x}', 'domain': 'local-b'}]}
            for index in range(pairs)]
    config['imap']['listen'] = f'127.0.0.1:{imap}'
    return config


def ports():
    # Keep both reservations alive until distinct numeric-loopback ports exist.
    with socket.socket() as smtp, socket.socket() as imap:
        smtp.bind(('127.0.0.1', 0))
        imap.bind(('127.0.0.1', 0))
        return smtp.getsockname()[1], imap.getsockname()[1]


def wait_ready(server, listeners):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        assert server.poll() is None, 'Fixture server exited before readiness'
        try:
            for port in listeners:
                with socket.create_connection(('127.0.0.1', port), timeout=.2):
                    pass
            return
        except OSError:
            time.sleep(.05)
    raise TimeoutError('Fixture listeners did not become ready.')


def validate(report, count, size, workers, status):
    assert isinstance(report, dict) and isinstance(report['metadata'], dict), 'invalid load report'
    assert report['requested'] == count and report['message_bytes'] == size and report['concurrency'] == workers, 'load workload mismatch'
    if status == 0:
        assert report['accepted'] == count and report['deferred'] == report['failed'] == 0, 'load acceptance incomplete'
        assert report['verified_headers'] == count and report['verified_fetch_samples'] > 0, 'load verification incomplete'
        assert not report.get('errors'), 'load verification failed'
    # A failing client can still emit a useful unsuccessful report. Its exit
    # status is preserved, and no success/cleanup claim is fabricated for it.
    bench.public_values(report)
    return report


def main(args):
    if len(args) > 5:
        raise bench.UsageError('Usage: scripts/bench.sh load-mail [COUNT [MESSAGE_BYTES [CONCURRENCY [NEW_REPORT_JSON [REPLICA_PAIRS]]]]]')
    count = int(args[0]) if args else 1000
    size = int(args[1]) if len(args) > 1 else 65536
    workers = int(args[2]) if len(args) > 2 else 16
    output = bench.new_output(args[3] if len(args) > 3 else bench.ROOT / '.fry-load-report.json')
    pairs = int(args[4]) if len(args) > 4 else 1
    assert 1 <= count <= 50000 and 1024 <= size <= 1048576 and 1 <= workers <= 128 and count * size <= 1073741824 and pairs in (1, 2, 4), 'Bounded workload required: count1..50000, bytes1024..1048576, concurrency1..128, total<=1GiB, pairs1/2/4'
    with bench.Run('mail-load') as run:
        database = run.fixture()
        environment = run.environment(FRY_DATABASE_URL=database.database_url,
                                      FRY_LOCAL_IMAP_PASSWORD='synthetic-load-only-password',
                                      FRY_LOAD_IMAP_PASSWORD='synthetic-load-only-password')
        smtp, imap = ports()
        config = configuration(run.root, smtp, imap, count, size, workers, pairs)
        config_path = run.root / 'config.json'
        config_path.write_text(json.dumps(config))
        fry, client = run.root / 'fry', run.root / 'mailload'
        run.run('go', 'build', '-o', str(fry), './cmd/fry', timeout=300, env=environment, log='build-fry.log')
        run.run('go', 'build', '-o', str(client), './internal/testutil/cmd/mailload', timeout=300,
                env=environment, log='build-client.log')
        run.run(str(fry), '-config', str(config_path), '-init-only', timeout=60, env=environment, log='init.log')
        server = run.start(str(fry), '-config', str(config_path), env=environment, log='server.log')
        wait_ready(server, (smtp, imap))
        result = run.run(str(client), '-smtp', f'127.0.0.1:{smtp}', '-imap', f'127.0.0.1:{imap}',
                         '-count', str(count), '-bytes', str(size), '-concurrency', str(workers),
                         timeout=150, env=environment, log='client.log', check=False)
        assert result.stdout.strip(), 'load client did not produce a report'
        report = validate(json.loads(result.stdout), count, size, workers, result.returncode)
        metadata = report['metadata']
        metadata['filesystem_type'] = run.run('stat', '-f', '-c', '%T', str(run.root), timeout=10).stdout.decode().strip()
        metadata['postgres_image'] = testdeps.image()
        metadata['replica_pairs'] = pairs
        placements = run.run('docker', 'exec', database.container, 'psql', '-U', 'postgres', '-d', 'fry_test',
                             '-At', '-c', PLACEMENT_SQL, timeout=30).stdout
        metadata['accepted_per_shard'] = json.loads(placements)
        metadata['per_member_capacity_bytes'] = config['member_capacity_bytes']
        metadata['aggregate_member_capacity_bytes'] = 2 * pairs * config['member_capacity_bytes']
        metadata['fixture'] = f'actual fry process; {2 * pairs} local hot members on shared filesystem; isolated PostgreSQL18 container; two aliases into one inbox; no outbound relay'
        metadata['server_memory_scope'] = 'fry process only; PostgreSQL container and load client excluded'
        try:
            for line in Path(f'/proc/{server.pid}/status').read_text().splitlines():
                if line.startswith('VmHWM:'):
                    metadata['server_peak_rss_kib'] = int(line.split()[1])
        except FileNotFoundError:
            metadata['server_peak_rss_kib'] = None
        bench.public_values(report, run.secrets)
    bench.private_json(output, report)
    print(json.dumps(report, indent=2))
    print('JSON report: ' + str(output), file=sys.stderr)
    return result.returncode if result.returncode >= 0 else 128 - result.returncode


if __name__ == '__main__':
    raise SystemExit(bench.entrypoint(main))
