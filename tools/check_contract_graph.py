#!/usr/bin/env python3
"""Check D0's symbolic dependencies/visibility; not a product record verifier."""
import json
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[1]
GRAPH = ROOT / 'docs/contracts/reference-graph.json'


def references(value):
    if isinstance(value, str) and value.startswith('@'):
        return {value[1:]}
    if isinstance(value, dict):
        return set().union(*(references(v) for v in value.values()))
    if isinstance(value, list):
        return set().union(*(references(v) for v in value))
    return set()


def validate(doc):
    records = {r['alias']: r for r in doc['records']}
    if len(records) != len(doc['records']):
        raise ValueError('duplicate alias')
    visited, visiting = set(), set()

    def visit(alias):
        if alias not in records:
            raise ValueError('unavailable reference: ' + alias)
        if alias in visiting:
            raise ValueError('hash cycle: ' + alias)
        if alias in visited:
            return
        visiting.add(alias)
        record = records[alias]
        refs = references(record['envelope'])
        if refs != set(record['hash_dependencies']):
            raise ValueError('dependency declaration differs from envelope: ' + alias)
        for ref in refs:
            visit(ref)
        visiting.remove(alias)
        visited.add(alias)

    for alias in records:
        visit(alias)
    entities = {}
    for alias, record in records.items():
        env = record['envelope']
        if env['schema_version'] != 1:
            raise ValueError('unsupported version')
        if env['kind'] in {'session', 'capability', 'intent', 'workspace'}:
            entity = env['payload']['id']
            if entity in entities:
                raise ValueError('conflicting entity creation: ' + entity)
            entities[entity] = alias
    if entities != doc['inventory']['entities']:
        raise ValueError('inventory resolution differs')
    for record in records.values():
        env, p = record['envelope'], record['envelope']['payload']
        if env['signer'] not in doc['inventory']['public_keys']:
            raise ValueError('unavailable signer')
        if env['kind'] == 'session':
            cap = records[entities[p['capability_id']]]['envelope']['payload']
            if cap['session_id'] != p['id'] or env['signer'] != 'root':
                raise ValueError('session/capability authentication mismatch')
    committed = set()
    operations = set()
    for alias in doc['published_receipts']:
        env = records[alias]['envelope']
        if env['kind'] != 'operation_receipt':
            raise ValueError('not a receipt')
        p = env['payload']
        if p['operation_id'] in operations:
            raise ValueError('incompatible receipt')
        operations.add(p['operation_id'])
        members = {v[1:] for v in p['object_hashes']}
        if alias in members or not members <= records.keys():
            raise ValueError('invalid receipt members')
        committed |= members
    for alias in committed:
        if not references(records[alias]['envelope']) <= committed:
            raise ValueError('committed group has pending dependency')
    # Replay only the example's workspace chain, without enumeration ordering.
    creation = records[entities['wsp_example']]
    state, previous = 'open', None
    events = {a: r['envelope']['payload'] for a, r in records.items()
              if a in committed and r['envelope']['kind'] == 'lifecycle_event'}
    allowed = {'open': {'proposed', 'closed'}, 'proposed': {'open', 'selected', 'closed'}}
    while events:
        next_events = [(a, p) for a, p in events.items() if p['previous_event_id'] == previous]
        if len(next_events) != 1:
            raise ValueError('forked or unavailable lifecycle predecessor')
        alias, p = next_events[0]
        if p['creation_hash'] != '@' + creation['alias'] or p['subject_id'] != 'wsp_example':
            raise ValueError('wrong subject creation')
        if p['from_state'] != state or p['to_state'] not in allowed.get(state, set()):
            raise ValueError('invalid transition')
        state, previous = p['to_state'], '@' + alias
        del events[alias]
    pending = {a for a, r in records.items()
               if r['envelope']['kind'] != 'operation_receipt' and a not in committed}
    if state != doc['expected']['workspace_state'] or pending != set(doc['expected']['pending']):
        raise ValueError('state or pending visibility differs')
    return len(records)


class ContractGraphTest(unittest.TestCase):
    def setUp(self):
        self.doc = json.loads(GRAPH.read_text())

    def test_complete_graph_and_shuffled_replay(self):
        self.assertEqual(validate(self.doc), 17)
        self.doc['records'].reverse()
        self.assertEqual(validate(self.doc), 17)

    def test_hash_cycle(self):
        r = self.doc['records'][0]
        r['envelope']['payload']['bad_hash'] = '@' + r['alias']
        r['hash_dependencies'].append(r['alias'])
        with self.assertRaisesRegex(ValueError, 'hash cycle'):
            validate(self.doc)

    def test_conflicting_creation(self):
        r = json.loads(json.dumps(self.doc['records'][0]))
        r['alias'] = 'duplicate_capability'
        self.doc['records'].append(r)
        with self.assertRaisesRegex(ValueError, 'conflicting entity'):
            validate(self.doc)

    def test_missing_receipt_does_not_commit_members(self):
        self.doc['published_receipts'].remove('receipt_propose')
        with self.assertRaisesRegex(ValueError, 'pending dependency'):
            validate(self.doc)

    def test_substituted_capability_holder(self):
        self.doc['records'][0]['envelope']['payload']['session_id'] = 'ses_intruder'
        with self.assertRaisesRegex(ValueError, 'authentication mismatch'):
            validate(self.doc)


if __name__ == '__main__':
    unittest.main(verbosity=2)
