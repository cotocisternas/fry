#!/usr/bin/env python3
"""Exercise compatibility refusal before opening storage, on disposable clones."""
import copy
import json
from pathlib import Path
import sys
import traceback

from scripts.smoke import offline_restore as fixture


class SchemaStartup(fixture.Rehearsal):
    def schema_snapshot(self):
        dump = fixture.run('docker', 'exec', self.db, 'pg_dump', '-U', 'postgres', '-d', 'fry_test',
                           '--schema-only', '--no-owner', '--no-acl', timeout=20).stdout
        # PostgreSQL 18 emits a fresh psql restriction token on each dump. It is
        # not schema state; retain every other byte of the DDL for comparison.
        return b'\n'.join(line for line in dump.splitlines()
                          if not line.startswith((b'\\restrict ', b'\\unrestrict ')))

    def schema_objects(self):
        # DDL/data equality alone could miss dropping and recreating objects.
        return self.sql("SELECT coalesce(json_agg(v ORDER BY name),'[]'::json) FROM "
                        "(SELECT c.relname AS name,c.oid::text AS oid,c.relfilenode::text AS filenode "
                        "FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
                        "WHERE n.nspname=current_schema() AND c.relname LIKE 'fry_%') v")

    def execute(self):
        self.prepare()
        fixture.run('go', 'build', '-o', str(self.root / 'bin/fry-migrate'), './cmd/fry-migrate', timeout=180)
        self.start_all()
        original = b'Subject: versioned installation\r\n\r\nAccepted before compatibility probes.\r\n'
        self.incoming(original)
        expected = self.mailbox_snapshot([original])
        self.stop_all()
        source_db, source_env = self.db, self.env.copy()
        original_authority = self.authority_snapshot()
        original_schema = self.schema_snapshot()
        original_objects = self.schema_objects()
        original_files = fixture.inventory(self.source)
        dump = fixture.run('docker', 'exec', self.db, 'pg_dump', '-U', 'postgres', '-d', 'fry_test', '--format=custom', '--no-owner', '--no-acl', timeout=30).stdout
        checked = fixture.run(str(self.root / 'bin/fry'), '-config', str(self.path), '-check-schema', env=self.env, timeout=20)
        assert b'FRY schema compatible' in checked.stderr
        assert self.authority_snapshot() == original_authority
        assert self.schema_snapshot() == original_schema
        assert fixture.inventory(self.source) == original_files
        repeated = fixture.run(str(self.root / 'bin/fry'), '-config', str(self.path), '-init-only', env=self.env, timeout=20, check=False)
        # Commands deliberately sanitize stderr. The same configuration passes
        # check-only immediately before and after refused fresh initialization;
        # the ledger/data, DDL and physical object identities must all survive.
        assert repeated.returncode != 0, 'fresh initialization accepted an existing schema'
        assert self.schema_objects() == original_objects, 'reinitialization replaced schema objects'
        fixture.run(str(self.root / 'bin/fry'), '-config', str(self.path), '-check-schema', env=self.env, timeout=20)
        assert self.authority_snapshot() == original_authority
        assert self.schema_snapshot() == original_schema

        # Fresh storage paths make an accidentally early Directory/Open visible.
        coordinator_root = self.root / 'coordinator-must-not-create'
        member_root = self.root / 'member-must-not-create'
        migration_root = self.root / 'migration-must-not-create'
        local = copy.deepcopy(self.config)
        local.pop('replica_tls')
        local['data_directory'] = str(coordinator_root)
        local['member_capacity_bytes'] = 64 << 20
        for member in local['members']:
            member.pop('url')
            member.pop('server_key')
        coordinator_path = self.root / 'probe-coordinator.json'
        coordinator_path.write_text(json.dumps(local))
        member = copy.deepcopy(self.member_configs[0])
        member['data_directory'] = str(member_root)
        member_path = self.root / 'probe-member.json'
        member_path.write_text(json.dumps(member))
        migration = json.loads(Path('examples/local-migration.json').read_text())
        migration['data_directory'] = str(migration_root)
        migration_path = self.root / 'probe-migration.json'
        migration_path.write_text(json.dumps(migration))
        probes = [('fry', coordinator_path), ('fry-member', member_path), ('fry-migrate', migration_path)]
        rejected = 0
        variants = {
            'unversioned': 'DROP TABLE fry_schema_version',
            'checksum': "UPDATE fry_schema_version SET checksum=repeat('0',64)",
            'missing-index': 'DROP INDEX fry_receipt_pack_schedule',
        }
        for label, mutation in variants.items():
            fixture.progress('checking refusal for ' + label + ' on a disposable database clone')
            self.database('schema-' + label)
            assert self.sql('SHOW server_version') == self.version
            fixture.run('docker', 'exec', '-i', self.db, 'pg_restore', '-U', 'postgres', '-d', 'fry_test', '--exit-on-error', '--single-transaction', '--no-owner', '--no-acl', data=dump, timeout=30)
            assert self.authority_snapshot() == original_authority
            # The untouched restored ledger must accept new database/OIDs before
            # this test deliberately corrupts only its disposable clone.
            fixture.run(str(self.root / 'bin/fry'), '-config', str(self.path), '-check-schema', env=self.env, timeout=20)
            self.sql(mutation)
            damaged = self.authority_snapshot()
            damaged_schema = self.schema_snapshot()
            damaged_objects = self.schema_objects()
            # Only the explicit schema mutation differs from the successful
            # check immediately above, independently of sanitized startup logs.
            incompatible = fixture.run(str(self.root / 'bin/fry'), '-config', str(self.path), '-check-schema', env=self.env, check=False, timeout=20)
            assert incompatible.returncode != 0, 'mutated schema passed compatibility check'
            for binary, path in probes:
                result = fixture.run(str(self.root / 'bin' / binary), '-config', str(path), env=self.env, check=False, timeout=20)
                assert result.returncode != 0, binary + ' did not reject incompatible schema before storage'
                rejected += 1
                assert not any(root.exists() for root in (coordinator_root, member_root, migration_root))
                assert self.authority_snapshot() == damaged, 'failed startup changed database authority'
                assert self.schema_snapshot() == damaged_schema, 'failed startup changed schema definitions'
                assert self.schema_objects() == damaged_objects, 'failed startup replaced schema objects'
                assert fixture.inventory(self.source) == original_files
            self.remove_database(self.db)
        self.db, self.env = source_db, source_env
        assert self.authority_snapshot() == original_authority
        self.start_all()
        assert self.mailbox_snapshot([original]) == expected
        fresh = b'Subject: after schema probes\r\n\r\nUnchanged installation still accepts.\r\n'
        self.incoming(fresh)
        self.mailbox_snapshot([original, fresh])
        self.stop_all(expected_closed_owners=4)
        return dict(passed=True, profile='versioned-schema-startup', rejected_startups=rejected,
                    negative_variants=list(variants), database_clones=3,
                    same_schema_restore_compatible=True, check_only_read_only=True,
                    storage_roots_not_created=True, original_authority_preserved=True,
                    schema_object_identity_preserved=True, incompatible_schema_checks=3,
                    originals_verified=2, postgres_version=self.version,
                    source_sha256={name: fixture.digest(Path(name)) for name in
                        ('scripts/smoke/schema_startup.py', 'scripts/smoke.sh',
                         'scripts/smoke/offline_restore.py', 'internal/dbschema/schema.go',
                         'internal/installation/schema_linux.go', 'db/schema/receiptstore.sql',
                         'db/schema/admin.sql', 'db/schema/maildirectory.sql',
                         'cmd/fry/main.go', 'cmd/fry-member/main.go', 'cmd/fry-migrate/main.go')},
                    binary_sha256={name: fixture.digest(self.root / 'bin' / name)
                                   for name in ('fry', 'fry-member', 'fry-migrate')})


if __name__ == '__main__':
    fixture.Rehearsal = SchemaStartup
    fixture.progress = lambda message: print('[schema startup] ' + message, flush=True)
    try:
        fixture.main()
    except Exception as error:
        message = str(error)
        for private in fixture.PRIVATE_VALUES:
            message = message.replace(private, '<REDACTED>')
        frame = traceback.extract_tb(error.__traceback__)[-1]
        location = f'{Path(frame.filename).name}:{frame.lineno} ({frame.name})'
        print('Schema startup rehearsal failed at ' + location + ': ' + type(error).__name__ + ': ' + message.split('\n')[0][:300], file=sys.stderr)
        raise SystemExit(1)
