#!/usr/bin/env python3
"""Disposable API/CLI diagnostics against actual mTLS member processes."""
import json
from pathlib import Path
import sys
import traceback

from scripts.smoke import offline_restore as fixture


class MemberStorage(fixture.Rehearsal):
    def diagnostic(self, member, after=None):
        env = self.env.copy()
        env['FRY_ADMIN_PASSWORD'] = self.admin_password
        args = [str(self.root / 'bin/fry-admin'), '-origin', self.origin,
                '-member', member, '-limit', '1']
        if after is not None:
            args.extend(('-after', after))
        result = fixture.run(*args, 'member-storage', env=env, timeout=15)
        value = json.loads(result.stdout)
        assert value['member'] == member and len(value['files']) <= 1
        assert value['sql_snapshot_at'] and value['runtime']['checked_at']
        return value

    def authority(self):
        return [self.sql('SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),\'[]\'::jsonb) FROM ' + table + ' t')
                for table in ('fry_member', 'fry_member_owner', 'fry_member_profile',
                              'fry_member_file', 'fry_member_file_lane', 'fry_target')]

    def execute(self):
        self.prepare()
        for member in self.member_configs:
            member.update(segment_max_records=1, segment_max_bytes=5 << 20)
        self.config['storage_lifecycle'].update(reclaim_hot=False, rotate_hot=False)
        self.start_all()
        first = b'Subject: diagnostics first\r\n\r\nAn original that fills a bounded file.\r\n'
        second = b'Subject: diagnostics second\r\n\r\nAn original after explicit owner reconnection.\r\n'
        self.incoming(first)
        self.mailbox_snapshot([first])
        fixture.progress('checking full current-file reservations through the real CLI/API')
        before = self.authority()
        for member in self.member_configs:
            result = self.diagnostic(member['id'])
            assert result['profile']['bounded'] and result['profile']['limits_frozen']
            assert result['current']['reserved_records'] == '1'
            assert result['current']['can_fit_max_record_snapshot'] is False
            assert result['runtime']['status'] == 'sampled'
            assert result['runtime']['frozen'] is False and result['runtime']['closed'] is False
            assert result['runtime']['closure_uncertain'] is False
            # Protobuf JSON omits absent optional fields. Empty strings and
            # false are still distinct from absence and must not pass here.
            assert result.get('next_file') is None
        assert self.authority() == before, 'diagnostics changed storage authority'

        self.rotate_members()
        fixture.progress('checking keyset pages after real rotation')
        before = self.authority()
        for member in self.member_configs:
            page = self.diagnostic(member['id'])
            assert page['current']['can_fit_max_record_snapshot'] is True
            assert page['current']['reserved_records'] == '0'
            assert page['next_file'] is not None
            following = self.diagnostic(member['id'], page['next_file'])
            assert following.get('next_file') is None
            files = page['files'] + following['files']
            assert len({entry['file_id'] for entry in files}) == 2
            assert {entry['state'] for entry in files} == {'current', 'sealed'}
        assert self.authority() == before, 'catalog inspection changed storage authority'

        fixture.progress('distinguishing stopped owners from a new unbound owner generation')
        member = self.member_configs[0]['id']
        self.stop_process(self.members[0])
        stopped = self.diagnostic(member)
        assert stopped['owner']['closed'] is True
        assert stopped['runtime']['status'] == 'unreachable'
        assert stopped['runtime'].get('closed') is None and stopped['runtime'].get('frozen') is None
        self.members[0] = self.start_member(0)
        before = self.authority()
        for _ in range(2):
            restarted = self.diagnostic(member)
            assert restarted['runtime']['status'] == 'generation_mismatch'
            assert restarted['runtime']['bound_owner_epoch'] == '1'
            assert restarted['runtime']['owner_epoch'] == '2'
            assert restarted['runtime'].get('current_file') is None
        assert self.authority() == before, 'diagnostics adopted or changed owner authority'

        self.stop_process(self.coordinator)
        self.start_coordinator()
        rebound = self.diagnostic(member)
        assert rebound['runtime']['status'] == 'sampled'
        assert rebound['runtime']['bound_owner_epoch'] == '2'
        self.incoming(second)
        self.mailbox_snapshot([first, second])
        self.stop_all(expected_closed_owners=3)
        return dict(passed=True, profile='synthetic-member-storage-diagnostics',
                    accepted_originals=2, configured_members=2,
                    cli_api_verified=True, paginated_files_verified=True,
                    authority_unchanged_by_diagnostics=True,
                    stopped_owner_unknown=True, generation_mismatch_not_adopted=True,
                    explicit_coordinator_reconnect_verified=True,
                    postgres_version=self.version,
                    source_sha256={name: fixture.digest(Path(name)) for name in
                        ('scripts/smoke/member_storage.py', 'internal/receiptstore/member_storage_linux.go',
                         'internal/receiptstore/member_storage_runtime_linux.go',
                         'internal/receiptstore/member_http_linux.go',
                         'internal/admin/memberstorage.go', 'internal/admintransport/http.go',
                         'cmd/fry-admin/main.go')},
                    binary_sha256={name: fixture.digest(self.root / 'bin' / name)
                                   for name in ('fry', 'fry-member', 'fry-admin')})


if __name__ == '__main__':
    fixture.Rehearsal = MemberStorage
    fixture.progress = lambda message: print('[member storage] ' + message, flush=True)
    try:
        fixture.main()
    except Exception as error:
        message = str(error)
        for private in fixture.PRIVATE_VALUES:
            message = message.replace(private, '<REDACTED>')
        frame = traceback.extract_tb(error.__traceback__)[-1]
        location = f'{Path(frame.filename).name}:{frame.lineno} ({frame.name})'
        print('Member storage rehearsal failed at ' + location + ': ' + type(error).__name__ + ': ' + message.split('\n')[0][:300], file=sys.stderr)
        raise SystemExit(1)
