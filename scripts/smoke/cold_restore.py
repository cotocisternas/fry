#!/usr/bin/env python3
"""Quiescent same-namespace cold restore using a disposable HTTP fixture.

No AWS copy/versioning, online backup, production fencing or secret-recovery claim.
"""
import base64
import copy
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import traceback

from scripts.smoke import offline_restore as fixture


class ColdRestore(fixture.Rehearsal):
    def start_objects(self, listen='127.0.0.1:0', state_in=None, state_out=None):
        self.round += 1
        log = self.root / f'objects-{self.round}.log'
        stream = log.open('wb')
        self.logs.append(stream)
        args = [str(self.root / 'bin/fry-s3-fixture'), '-listen', listen]
        if state_in is not None:
            args.extend(('-state-in', str(state_in)))
        if state_out is not None:
            args.extend(('-state-out', str(state_out)))
        process = subprocess.Popen(args, stdout=stream, stderr=stream, env=self.env)
        self.processes.append(process)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            text = log.read_text()
            assert process.poll() is None, 'object fixture exited before readiness'
            match = re.search(r'FRY S3 fixture listening on (http://127\.0\.0\.1:\d+)', text)
            if match:
                return process, match[1]
            time.sleep(.05)
        raise AssertionError('object fixture startup timeout')

    def wait_sql(self, query, expected, description):
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            if self.sql(query) == str(expected):
                return
            time.sleep(.1)
        raise AssertionError(description)

    def wait_cold(self, live):
        self.wait_sql("SELECT count(*) FROM fry_receipt WHERE lifecycle='live' AND pack_id IS NOT NULL", live, 'live originals were not packed')
        self.wait_sql('SELECT count(*) FROM fry_target WHERE NOT bytes_removed', 0, 'hot bytes were not reclaimed')
        self.wait_sql("SELECT count(*) FROM fry_pack WHERE state NOT IN ('published','retired') OR NOT staging_closed", 0, 'pack construction or retirement remains unresolved')

    def stop_mail(self, closed_owners):
        self.stop_process(self.coordinator)
        for member in self.members:
            self.stop_process(member)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0'
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE closed') == str(closed_owners)
        assert self.sql('SELECT count(*) FROM fry_read_pin') == '0'
        assert self.sql('SELECT count(*) FROM fry_member_read WHERE NOT closed') == '0'
        assert self.sql("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' AND pid<>pg_backend_pid()") == '0'
        assert self.sql("SELECT count(*) FROM fry_pack WHERE state NOT IN ('published','retired') OR NOT staging_closed") == '0'

    def pack_authority(self):
        return json.loads(self.sql("SELECT COALESCE(jsonb_agg(jsonb_build_object('key',object_key,'state',state,'size',image_size,'digest',encode(image_sha256,'hex'),'etag',etag) ORDER BY object_key),'[]'::jsonb) FROM fry_pack"))

    @staticmethod
    def check_objects(path, packs, endpoint):
        state = json.loads(path.read_text())
        assert state['version'] == 1 and state['endpoint'] == endpoint
        objects = {obj['key']: obj for obj in state['objects']}
        assert len(objects) == len(state['objects'])
        assert set(objects) == {pack['key'] for pack in packs}, 'object archive omitted or invented a pack key'
        published = retired = 0
        for pack in packs:
            obj = objects[pack['key']]
            body = base64.b64decode(obj['body'], validate=True)
            assert hashlib.sha256(body).hexdigest() == obj['digest']
            if pack['state'] == 'published':
                published += 1
                assert not obj['retired'] and len(body) == pack['size']
                assert obj['digest'] == pack['digest'] and obj['etag'] == pack['etag']
            else:
                assert pack['state'] == 'retired'
                retired += 1
                # The marker's ETag differs from a formerly published image's
                # historical DB ETag. Preserve it literally, never recompute it.
                assert obj['retired'] and body == b''
        assert published and retired, 'fixture must cover both live images and retired markers'
        return state

    def execute(self):
        self.prepare()
        fixture.run('go', 'build', '-o', str(self.root / 'bin/fry-s3-fixture'), './internal/testutil/cmd/s3fixture', timeout=180)
        source_state = self.root / 'source-object-state.json'
        objects, endpoint = self.start_objects(state_out=source_state)
        scratch = self.root / 'source-pack-scratch'
        self.config['storage_lifecycle'] = dict(
            objects=dict(region='sa-east-1', bucket='fry-test', endpoint=endpoint,
                         use_path_style=True, loopback_anonymous=True),
            scratch_directory=str(scratch), max_pack_bytes=2 << 20,
            max_staging_bytes=8 << 20, pack_lease_seconds=3,
            interval_seconds=1, work_timeout_seconds=20, max_actions=5,
            max_receipts=32, reclaim_hot=True, reclaim_interval_seconds=1)
        self.start_all()
        originals = [f'Subject: cold restore {i}\r\n\r\nExact retained cold original {i}.\r\n'.encode() for i in range(2)]
        expired = b'Subject: expire before cold backup\r\n\r\nOriginal removed through normal lifecycle.\r\n'
        for body in originals + [expired]:
            self.incoming(body)
        self.mailbox_snapshot(originals + [expired])
        archived = b'Subject: dated cold archive\r\n\r\nPreserve folder state and original bytes.\r\n'
        with self.imap() as client:
            assert client.create('Archive')[0] == 'OK'
            assert client.append('Archive', '(\\Flagged)', '"12-Jan-2001 03:04:05 +0000"', archived)[0] == 'OK'
            assert client.select('INBOX')[0] == 'OK'
            assert client.uid('STORE', '1', '+FLAGS.SILENT', '(\\Seen)')[0] == 'OK'
        fixture.progress('packing originals and proving both hot copies have been reclaimed')
        self.wait_cold(4)
        self.mailbox_snapshot(originals + [expired])
        with self.imap() as client:
            assert client.select('INBOX')[0] == 'OK'
            assert client.uid('STORE', '3', '+FLAGS.SILENT', '(\\Deleted)')[0] == 'OK'
            assert client.expunge()[0] == 'OK'
        sha = hashlib.sha256(expired).hexdigest()
        # Simulate elapsed recovery time only. Real expunge, expiry, compaction
        # and object retirement establish all removal and marker authority.
        self.sql("UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 second' WHERE kind='recovery' AND receipt=(SELECT id FROM fry_receipt WHERE encode(sha256,'hex')='" + sha + "')")
        self.wait_sql("SELECT count(*) FROM fry_receipt WHERE lifecycle='removed'", 1, 'expired original did not finish physical removal')
        self.wait_cold(3)
        expected_inbox = self.mailbox_snapshot(originals)
        expected_archive = self.mailbox_snapshot([archived], 'Archive')
        fixture.progress('closing every writer and capturing database, roots, scratch and exact objects')
        self.stop_mail(2)
        packs = self.pack_authority()
        self.stop_process(objects)
        source_objects = self.check_objects(source_state, packs, endpoint)
        state = self.authority_snapshot()
        source_db = self.db
        backup = self.root / 'cold-backup'
        backup.mkdir(mode=0o700)
        dump = fixture.run('docker', 'exec', source_db, 'pg_dump', '-U', 'postgres', '-d', 'fry_test', '--format=custom', '--no-owner', '--no-acl', timeout=30).stdout
        (backup / 'database.dump').write_bytes(dump)
        shutil.copytree(self.source, backup / 'members', copy_function=shutil.copy2)
        shutil.copytree(scratch, backup / 'scratch', copy_function=shutil.copy2)
        shutil.copy2(source_state, backup / 'objects.json')
        (backup / 'pack-authority.json').write_text(json.dumps(packs, sort_keys=True))
        (backup / 'configuration.json').write_text(json.dumps(
            dict(coordinator=self.config, members=self.member_configs), sort_keys=True))
        assert fixture.inventory(self.source) == fixture.inventory(backup / 'members')
        assert fixture.inventory(scratch) == fixture.inventory(backup / 'scratch')
        manifest = fixture.inventory(backup)
        fixture.verify_archive(backup, manifest)
        for mode in ('missing-object-archive', 'corrupt-object-archive'):
            trial = self.root / mode
            shutil.copytree(backup, trial)
            if mode.startswith('missing'):
                (trial / 'objects.json').unlink()
            else:
                with (trial / 'objects.json').open('r+b') as stream:
                    first = stream.read(1)
                    stream.seek(0)
                    stream.write(bytes([first[0] ^ 1]))
            try:
                fixture.verify_archive(trial, manifest)
            except AssertionError:
                pass
            else:
                raise AssertionError('invalid object archive passed verification')
            shutil.rmtree(trial)

        fixture.progress('destroying original byte stores and database before same-namespace restore')
        self.remove_database(source_db)
        shutil.rmtree(self.source)
        shutil.rmtree(scratch)
        source_state.unlink()
        assert all(process.poll() == 0 for process in self.processes)
        assert not self.source.exists() and not scratch.exists() and not source_state.exists()
        self.database('cold-restored')
        assert self.sql('SHOW server_version') == self.version
        fixture.verify_archive(backup, manifest)
        fixture.run('docker', 'exec', '-i', self.db, 'pg_restore', '-U', 'postgres', '-d', 'fry_test', '--exit-on-error', '--single-transaction', '--no-owner', '--no-acl', data=(backup / 'database.dump').read_bytes(), timeout=30)
        assert self.authority_snapshot() == state, 'restored database tables/sequences differ before startup'
        restored = self.root / 'restored-members'
        restored_scratch = self.root / 'restored-scratch'
        shutil.copytree(backup / 'members', restored, copy_function=shutil.copy2)
        shutil.copytree(backup / 'scratch', restored_scratch, copy_function=shutil.copy2)
        assert fixture.inventory(restored) == fixture.inventory(backup / 'members')
        assert fixture.inventory(restored_scratch) == fixture.inventory(backup / 'scratch')
        assert self.check_objects(backup / 'objects.json', self.pack_authority(), endpoint) == source_objects
        configuration = json.loads((backup / 'configuration.json').read_text())
        self.config = configuration['coordinator']
        self.member_configs = configuration['members']
        final_state = self.root / 'restored-object-state.json'
        objects, restored_endpoint = self.start_objects(endpoint.removeprefix('http://'), backup / 'objects.json', final_state)
        assert restored_endpoint == endpoint
        for i in range(2):
            self.member_configs[i]['data_directory'] = str(restored / ('member-' + str(i)))
        self.config['storage_lifecycle']['scratch_directory'] = str(restored_scratch)
        self.members = [self.start_member(i) for i in range(2)]

        # A reachable but different fixture must fail the persisted namespace
        # binding, not merely fail to connect. Never edit namespace authority.
        wrong_objects, wrong_endpoint = self.start_objects()
        wrong = copy.deepcopy(self.config)
        wrong['storage_lifecycle']['objects']['endpoint'] = wrong_endpoint
        wrong_path = self.root / 'wrong-namespace.json'
        wrong_path.write_text(json.dumps(wrong))
        before_namespace = self.sql('SELECT namespace FROM fry_object_namespace')
        result = fixture.run(str(self.root / 'bin/fry'), '-config', str(wrong_path), env=self.env, check=False, timeout=30)
        assert result.returncode != 0 and b'immutable object namespace mismatch' in result.stderr, 'different reachable namespace was not explicitly rejected'
        assert self.sql('SELECT namespace FROM fry_object_namespace') == before_namespace
        self.stop_process(wrong_objects)
        self.start_coordinator()
        fixture.progress('reading restored cold-only originals while both hot members are stopped')
        for member in self.members:
            self.stop_process(member)
        assert self.sql('SELECT count(*) FROM fry_target WHERE NOT bytes_removed') == '0'
        assert self.mailbox_snapshot(originals) == expected_inbox
        assert self.mailbox_snapshot([archived], 'Archive') == expected_archive
        assert self.sql("SELECT count(*) FROM fry_receipt WHERE lifecycle='removed' AND encode(sha256,'hex')='" + sha + "'") == '1'
        self.stop_process(self.coordinator)
        self.members = [self.start_member(i) for i in range(2)]
        self.start_coordinator()
        fresh = b'Subject: fresh after cold restore\r\n\r\nNew original must pack normally.\r\n'
        self.incoming(fresh)
        self.wait_cold(4)
        self.mailbox_snapshot(originals + [fresh])
        assert self.mailbox_snapshot([archived], 'Archive') == expected_archive
        self.stop_mail(6)
        final_packs = self.pack_authority()
        self.stop_process(objects)
        final_objects = self.check_objects(final_state, final_packs, endpoint)
        old = {obj['key']: obj for obj in source_objects['objects']}
        new = {obj['key']: obj for obj in final_objects['objects']}
        assert all(new.get(key) == obj for key, obj in old.items()), 'existing image or retired marker changed after restore'
        assert final_objects['serial'] > source_objects['serial'] and len(new) > len(old), 'restored fixture did not advance its opaque ETag sequence'
        return dict(passed=True, profile='quiescent-same-namespace-fixture-cold-restore',
                    restored_live_originals=3, fresh_originals=1, expired_originals_still_removed=1,
                    cold_reads_with_both_members_stopped=True, complete_authority_restored=True,
                    scratch_identity_preserved=True, exact_object_metadata_preserved=True,
                    retired_markers_preserved=sum(obj['retired'] for obj in source_objects['objects']),
                    negative_archives_rejected=2, changed_namespace_rejected=True,
                    archive_files=sum(item['type'] == 'file' for item in manifest.values()),
                    postgres_version=self.version,
                    source_sha256={name: fixture.digest(Path(name)) for name in
                        ('scripts/smoke/cold_restore.py', 'scripts/smoke.sh',
                         'scripts/smoke/offline_restore.py', 'examples/local-admin.json',
                         'db/schema/receiptstore.sql', 'db/schema/admin.sql',
                         'internal/testutil/s3fixture/state.go',
                         'internal/testutil/s3fixture/server.go', 'internal/testutil/cmd/s3fixture/main.go')},
                    binary_sha256={name: fixture.digest(self.root / 'bin' / name)
                                   for name in ('fry', 'fry-member', 'fry-admin', 'fry-s3-fixture')})


if __name__ == '__main__':
    fixture.Rehearsal = ColdRestore
    fixture.progress = lambda message: print('[cold restore] ' + message, flush=True)
    try:
        fixture.main()
    except Exception as error:
        message = str(error)
        for private in fixture.PRIVATE_VALUES:
            message = message.replace(private, '<REDACTED>')
        frame = traceback.extract_tb(error.__traceback__)[-1]
        location = f'{Path(frame.filename).name}:{frame.lineno} ({frame.name})'
        print('Cold restore rehearsal failed at ' + location + ': ' + type(error).__name__ + ': ' + message.split('\n')[0][:300], file=sys.stderr)
        raise SystemExit(1)
