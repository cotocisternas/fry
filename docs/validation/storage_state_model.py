"""Bounded executable specification, NOT FRY storage implementation or an AWS test.

Explore one receipt, two hot-copy AZs, one immutable pack key, two worker epochs,
one reader pin, atomic durable DB transactions and at most one AZ loss. Check
safety after every event. Mutants must expose the races the guards prevent.
"""
from collections import deque
from dataclasses import dataclass, replace, asdict
from pathlib import Path
import json

@dataclass(frozen=True)
class State:
    phase: str = 'absent'
    hot: int = 0
    proofs: int = 0
    lost: int = 0
    database: bool = True
    degraded: bool = False
    accepted_mode: str = ''
    ack: bool = False
    job: str = 'none'
    epoch: int = 0
    published_epoch: int = -1
    object: str = 'absent'
    pinned: bool = False

@dataclass(frozen=True)
class Rules:
    guard_attempt: bool = True
    guard_epoch: bool = True
    retain_tombstone: bool = True


def transitions(s, rules):
    def event(name, **changes):
        return name, replace(s, **changes)
    if s.database:
        yield event('database unavailable (durable rows remain)', database=False)
    else:
        yield event('database recovered', database=True)
    if not s.lost:
        for bit, name in ((1, 'A'), (2, 'B')):
            yield event('lose AZ ' + name, lost=bit, hot=s.hot & ~bit)
    if s.database and s.lost and not s.degraded:
        yield event('activate recorded reduced-protection profile', degraded=True)
    if s.database and s.phase == 'absent':
        yield event('register attempt before byte I/O', phase='preparing')
    if s.phase == 'preparing':
        for bit, name in ((1, 'A'), (2, 'B')):
            if not s.lost & bit and not s.hot & bit:
                yield event('fsync original in AZ ' + name,
                            hot=s.hot | bit, proofs=s.proofs | bit)
        if s.database:
            yield event('abort attempt under guard', phase='aborted')
    allowed_phase = s.phase == 'preparing' or (
        not rules.guard_attempt and s.phase == 'aborted')
    if s.database and allowed_phase:
        if not s.lost and s.proofs == 3:
            yield event('commit normal receipt using durability receipts',
                        phase='accepted', accepted_mode='normal')
        elif s.degraded and s.proofs & ~s.lost:
            yield event('commit explicitly degraded receipt',
                        phase='accepted', accepted_mode='degraded')
    if s.database and s.phase == 'accepted' and not s.ack:
        yield event('deliver SMTP success (may have been delayed)', ack=True)
    if s.database and s.phase == 'accepted' and s.ack:
        yield event('final required reference ends', phase='expired')
    if s.database and s.phase == 'accepted' and s.hot and s.job == 'none':
        yield event('register pack and start bounded upload', job='building')
    if s.database and s.job == 'building' and s.epoch == 0:
        yield event('lease takeover fences epoch zero', epoch=1)
    # A PUT started earlier can finish even after lease loss or retirement.
    if s.job != 'none' and s.object == 'absent':
        yield event('old in-flight create-only PUT completes', object='body')
    if s.database and s.job == 'building' and s.object == 'body' and s.phase == 'accepted':
        for writer_epoch in (0, 1):
            if writer_epoch <= s.epoch and (not rules.guard_epoch or writer_epoch == s.epoch):
                yield event(f'publish pack from epoch {writer_epoch}',
                            job='published', published_epoch=writer_epoch)
    if s.database and s.phase == 'accepted' and s.job == 'published' and not s.pinned:
        yield event('pin published body for read', pinned=True)
    if s.pinned:
        yield event('reader closes actual handle', pinned=False)
    if s.database and s.phase == 'expired' and s.job in ('building', 'published'):
        yield event('retire pack publication rights', job='retiring')
    if s.database and s.job == 'retiring' and not s.pinned:
        yield event('replace body with permanent empty marker' if rules.retain_tombstone
                    else 'UNSAFE delete key while old PUT may finish',
                    object='tombstone' if rules.retain_tombstone else 'absent', job='retired')
    if s.database and (s.phase in ('aborted', 'expired') or
                       (s.phase == 'accepted' and s.job == 'published' and s.object == 'body')):
        for bit, name in ((1, 'A'), (2, 'B')):
            if s.hot & bit:
                yield event('reclaim safe hot copy ' + name, hot=s.hot & ~bit)


def violation(s):
    if s.phase == 'accepted' and not (s.hot or (s.job == 'published' and s.object == 'body')):
        return 'accepted live receipt has no recoverable bytes'
    if s.ack and s.phase not in ('accepted', 'expired'):
        return 'success without durable acceptance'
    if s.phase == 'accepted' and s.accepted_mode == 'normal' and s.proofs != 3:
        return 'normal acceptance lacked two durable failure domains'
    if s.job == 'published' and s.published_epoch != s.epoch:
        return 'stale worker published after ownership changed'
    if s.pinned and s.object != 'body':
        return 'body retired while reader still owns a handle'
    if s.job == 'retired' and s.object == 'body':
        return 'late upload resurrected retired message bytes'
    return None


def explore(rules=Rules()):
    root = State()
    parents = {root: None}
    queue = deque([root])
    edges = 0
    coverage = set()
    while queue:
        current = queue.popleft()
        reason = violation(current)
        if reason:
            trace = []
            at = current
            while parents[at] is not None:
                previous, event = parents[at]
                trace.append(event)
                at = previous
            return {'passed': False, 'states': len(parents), 'transitions': edges,
                    'violation': reason, 'trace': list(reversed(trace)), 'state': asdict(current)}
        if current.ack: coverage.add('smtp_success')
        if current.phase == 'accepted' and current.lost: coverage.add('accepted_survives_zone_loss')
        if current.accepted_mode == 'degraded': coverage.add('explicit_degraded_acceptance')
        if current.job == 'retired': coverage.add('pack_retirement')
        if current.epoch == 1 and current.job == 'published': coverage.add('takeover_publication')
        if current.pinned and current.phase == 'expired': coverage.add('expiry_with_active_reader')
        if current.phase == 'accepted' and not current.database: coverage.add('database_outage_after_commit')
        for event, nxt in transitions(current, rules):
            edges += 1
            if nxt not in parents:
                parents[nxt] = (current, event)
                queue.append(nxt)
    expected = {'smtp_success', 'accepted_survives_zone_loss', 'explicit_degraded_acceptance',
                'pack_retirement', 'takeover_publication', 'expiry_with_active_reader',
                'database_outage_after_commit'}
    assert coverage == expected, coverage
    return {'passed': True, 'states': len(parents), 'transitions': edges, 'coverage': sorted(coverage)}


def check_mixed_pack():
    """Explore independent build/expiry/publication ordering with two messages.

    A second live message prevents whole-pack destruction. Publication must compare
    the exact membership/version snapshot; a reference added while copying also
    changes that snapshot, rather than being guessed from an old reference count.
    """
    from itertools import permutations
    checked = 0
    for order in permutations(('snapshot', 'expire_a', 'publish')):
        live = {'a', 'b'}
        generation = 0
        snapshot = None
        new_pack = None
        for action in order:
            if action == 'snapshot':
                snapshot = (frozenset(live), generation)
            elif action == 'expire_a':
                live.remove('a')
                generation += 1
            elif snapshot == (frozenset(live), generation):
                new_pack = set(live)
        # If publication preceded expiry, eligibility cleanup must still remove A;
        # never regard that publication alone as physical deletion.
        if new_pack is not None:
            assert 'b' in new_pack
            if 'a' in new_pack and 'a' not in live:
                cleanup_debt = new_pack - live
                assert cleanup_debt == {'a'}
        # Regardless of timing, old pack can be retired only after B has a successor.
        can_retire_old = new_pack is not None and live <= new_pack
        if can_retire_old:
            assert live <= new_pack
        checked += 1
    return {'orders_checked': checked, 'scope': 'two-record membership snapshot and surviving reference'}


def main():
    good = explore()
    assert good['passed'], good
    mutants = {}
    for name, rules in {
        'missing_attempt_guard': Rules(guard_attempt=False),
        'missing_epoch_guard': Rules(guard_epoch=False),
        'delete_instead_of_retained_marker': Rules(retain_tombstone=False),
    }.items():
        result = explore(rules)
        assert not result['passed'], f'mutation went undetected: {name}'
        mutants[name] = result
    report = {
        'scope': 'bounded abstract state exploration; not production implementation, timing, disk, PostgreSQL, or S3 validation',
        'correct_protocol': good, 'mutation_counterexamples': mutants,
        'mixed_pack': check_mixed_pack(),
        'assumptions': ['atomic durable database transactions', 'documented atomic conditional object writes',
                        'truthful durable byte confirmations', 'at most one AZ loss',
                        'actual reader handle closes when modeled close occurs'],
        'not_tested': ['real fsync/partial writes', 'SQL isolation/locks', 'S3/IAM/versioning semantics',
                       'network partitions or operator authentication', 'five-minute recovery',
                       '24-hour cleanup liveness', 'throughput, memory, and cost'],
    }
    Path(__file__).with_name('storage-state-results.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps({'correct_protocol': good,
                      'mutants_detected': {k:v['violation'] for k,v in mutants.items()},
                      'mixed_pack': report['mixed_pack']}, indent=2))

if __name__ == '__main__':
    main()
