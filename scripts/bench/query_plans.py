#!/usr/bin/env python3
"""Bounded synthetic PostgreSQL plans for the exact production mailbox SQL.

No Store is opened and no storage acceptance/durability claim is made. Only a
fresh uniquely named PostgreSQL fixture and generated authority rows are used.
"""
import hashlib
import json
import os
from pathlib import Path
import re

from scripts.lib import benchmark as bench
from scripts.lib import testdeps

CUTOFF = '2026-10-17 12:00:00+00'


def run(*args, timeout=30, check=True, data=None):
    return bench.command(*args, timeout=timeout, check=check, data=data)


def org(number):
    return f'{4_000_000 + number:032x}'


def walk(plan):
    yield plan
    for child in plan.get('Plans', []):
        yield from walk(child)


def summarize(value):
    plan = value[0]['Plan']
    nodes = list(walk(plan))
    return {'execution_ms': value[0]['Execution Time'], 'planning_ms': value[0]['Planning Time'],
            'actual_rows': plan['Actual Rows'], 'shared_hit_blocks': plan.get('Shared Hit Blocks', 0),
            'shared_read_blocks': plan.get('Shared Read Blocks', 0),
            'sorts': [{'type': n['Node Type'], 'method': n.get('Sort Method'), 'rows': n['Actual Rows'],
                       'input_rows': sum(c['Actual Rows'] * c['Actual Loops'] for c in n.get('Plans', [])),
                       'relations': sorted({c['Relation Name'] for c in walk(n) if 'Relation Name' in c}),
                       'keys': n.get('Sort Key'),
                       'space_kib': n.get('Sort Space Used'), 'space_type': n.get('Sort Space Type')}
                      for n in nodes if 'Sort' in n['Node Type']],
            'scans': [{'relation': n.get('Relation Name'), 'type': n['Node Type'],
                       'index': n.get('Index Name'), 'rows': n['Actual Rows'], 'loops': n['Actual Loops'],
                       'removed_by_filter': n.get('Rows Removed by Filter', 0),
                       'shared_hit_blocks': n.get('Shared Hit Blocks', 0)}
                      for n in nodes if 'Scan' in n['Node Type']]}


class Check:
    def __init__(self, owner):
        self.owner = owner
        self.name = None
        self.database = None
        self.schema = Path('db/schema/receiptstore.sql').read_text()

    def sql(self, query, timeout=20):
        return run('docker', 'exec', '-i', self.name, 'psql', '-X', '-qAt', '-U', 'postgres', '-d', 'fry_test',
                   '-v', 'ON_ERROR_STOP=1', data=query.encode(), timeout=timeout).stdout.decode().strip()

    def setup(self):
        endpoint = run('docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}').stdout.decode().strip()
        assert endpoint.startswith('unix://'), 'local Unix-socket Docker context required'
        assert not os.environ.get('DOCKER_HOST') or os.environ['DOCKER_HOST'].startswith('unix://')
        self.database = self.owner.fixture()
        self.name = self.database.container
        self.sql("SET statement_timeout='10s';\n" + self.schema, timeout=30)
        self.sql("""
SET statement_timeout='10s';
CREATE TEMP TABLE fixture AS
SELECT n, CASE WHEN n<=18000 THEN 1 ELSE 2+(n-18001)/100 END AS tenant,
       lpad(to_hex(1000000+n),32,'0') AS receipt,
       lpad(to_hex(2000000+n),32,'0') AS attempt
FROM generate_series(1,20000) n;
INSERT INTO fry_ingest_shard(id,member_a,member_b)
VALUES(lpad('1',32,'0'),lpad('1',32,'0'),lpad('2',32,'0'));
INSERT INTO fry_attempt(ingest_shard,id,receipt_id,organization,envelope,state,epoch,profile_epoch,lease_until,recovery_seconds)
SELECT lpad('1',32,'0'),attempt,receipt,lpad(to_hex(4000000+tenant),32,'0'),'{}','accepted',1,1,
       '2026-10-18 12:00:00+00',86400 FROM fixture;
INSERT INTO fry_receipt(id,attempt,organization,received_at,raw_size,sha256,profile_epoch,protection)
SELECT receipt,attempt,lpad(to_hex(4000000+tenant),32,'0'),
       '2026-10-16 12:00:00+00'::timestamptz+n*interval '1 millisecond',0,decode(repeat('00',32),'hex'),1,'two-domain'
FROM fixture;
INSERT INTO fry_inbox(id,organization)
SELECT DISTINCT lpad(to_hex(3000000+tenant),32,'0'),lpad(to_hex(4000000+tenant),32,'0') FROM fixture;
INSERT INTO fry_work(receipt,inbox,kind,organization,received_at,available_at,failures)
SELECT f.receipt,lpad(to_hex(3000000+f.tenant),32,'0'),'publish-mailbox',r.organization,r.received_at,
       CASE WHEN f.n<=17000 THEN '2026-10-18 12:00:00+00'::timestamptz
            ELSE '2026-10-17 11:00:00+00'::timestamptz END,
       CASE WHEN f.n<=17000 THEN 3 ELSE 0 END
FROM fixture f JOIN fry_receipt r ON r.id=f.receipt;
ANALYZE fry_work;
ANALYZE fry_receipt;
ANALYZE fry_inbox;
""", timeout=30)

    def explain(self, name, sql, parameters, types, generic=False):
        mode = 'force_generic_plan' if generic else 'force_custom_plan'
        query = ("BEGIN READ ONLY; SET LOCAL statement_timeout='3s'; SET LOCAL plan_cache_mode='" + mode + "';\n"
                 + 'PREPARE plan_query(' + types + ') AS ' + sql + ';\n'
                 + 'EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE plan_query(' + parameters + ');\nROLLBACK;')
        value = json.loads(self.sql(query, timeout=10))
        result = {'name': name, 'plan_mode': mode, 'sql': sql, 'parameters': parameters,
                  'summary': summarize(value), 'explain': value}
        summary = result['summary']
        warnings = []
        queue_sorts = [v for v in summary['sorts'] if 'fry_work' in v['relations']]
        if queue_sorts:
            warnings.append('queue sort present: ' + ', '.join(v['method'] or v['type'] for v in queue_sorts))
        expected_limit = 64 if name.startswith('batch-') else 1
        for scan in summary['scans']:
            touched = (scan['rows'] + scan['removed_by_filter']) * scan['loops']
            if scan['relation'] == 'fry_work' and touched > expected_limit:
                warnings.append('work scan exceeds requested valid-fixture bound: ' + str(touched))
            if scan['relation'] == 'fry_receipt' and (touched > expected_limit or scan['loops'] > expected_limit):
                warnings.append('receipt lookups exceed requested valid-fixture bound: ' + str(max(touched, scan['loops'])))
        if name.startswith('batch-') and summary['actual_rows'] != 64:
            warnings.append('valid due batch did not return exactly requested64')
        result['warnings'] = warnings
        print(json.dumps({'name': name, **summary, 'warnings': warnings}), flush=True)
        return result


def generated_query(source, name):
    matches = re.findall(r'^const ' + re.escape(name) + r' = `([^`]+)`', source, re.M)
    if len(matches) != 1:
        raise ValueError('Expected exactly one generated query: ' + name)
    annotation, separator, sql = matches[0].partition('\n')
    if not separator or not annotation.startswith('-- name: ' + name + ' '):
        raise ValueError('Missing sqlc query annotation: ' + name)
    return sql.strip()


def queries():
    # Inspect the constants actually executed by sqlc's generated methods, not
    # a copied query or the former inline SQL in mailbox_linux.go.
    paths = ('internal/receiptstore/dbgen/mailbox_variants.sql.go',
             'internal/receiptstore/dbgen/receiptstore.sql.go')
    sources = {path: (bench.ROOT / path).read_text() for path in paths}
    hashes = {path: hashlib.sha256(source.encode()).hexdigest()
              for path, source in sources.items()}
    return (generated_query(sources[paths[0]], 'NextMailboxOrganization'),
            generated_query(sources[paths[0]], 'WrappedMailboxOrganization'),
            generated_query(sources[paths[1]], 'StoreSelectMailboxTurnReadWork'), hashes)


def main(args):
    if len(args) > 1:
        raise bench.UsageError('Usage: scripts/bench.sh query-plans [NEW_REPORT_JSON]')
    report_path = bench.new_output(args[0]) if args else None
    candidate, wrap, batch, source_hash = queries()
    with bench.Run('query-plans') as owner:
        check = Check(owner)
        check.setup()
        # Candidate2 includes many old but not-yet-due rows before the first due
        # work. Candidate-after-hot proves a small org can be selected next.
        plans = [check.explain('candidate-hot', candidate, "'" + CUTOFF + "', ''", 'timestamptz,text'),
                 check.explain('candidate-hot-generic', candidate, "'" + CUTOFF + "', ''", 'timestamptz,text', generic=True),
                 check.explain('candidate-after-hot', candidate, "'" + CUTOFF + "', '" + org(1) + "'", 'timestamptz,text'),
                 check.explain('candidate-past-end', candidate, "'" + CUTOFF + "', '" + org(21) + "'", 'timestamptz,text'),
                 check.explain('candidate-wrap', wrap, "'" + CUTOFF + "', '" + org(21) + "'", 'timestamptz,text'),
                 check.explain('batch-hot', batch, "'" + org(1) + "', '" + CUTOFF + "',64", 'text,timestamptz,integer'),
                 check.explain('batch-small', batch, "'" + org(2) + "', '" + CUTOFF + "',64", 'text,timestamptz,integer'),
                 check.explain('batch-hot-generic', batch, "'" + org(1) + "', '" + CUTOFF + "',64", 'text,timestamptz,integer', generic=True)]
        report = {'profile': 'synthetic-authority-query-plans', 'rows': 20000, 'organizations': 21,
                  'dominant_org_rows': 18000, 'dominant_org_future_prefix': 17000,
                  'batch_limit': 64, 'cutoff': CUTOFF, 'query_source_sha256': source_hash,
                  'schema_sha256': hashlib.sha256(check.schema.encode()).hexdigest(),
                  'postgres_image': testdeps.image(), 'postgres_version': check.sql('SELECT version();'),
                  'plans': plans}
        bench.public_values(report, owner.secrets)
    if report_path:
        bench.private_json(report_path, report)
    if any(value['warnings'] for value in plans):
        print('RED FLAG: queue sort or valid-fixture scan bound violation; inspect retained report.', flush=True)
        return 1


if __name__ == '__main__':
    raise SystemExit(bench.entrypoint(main))
