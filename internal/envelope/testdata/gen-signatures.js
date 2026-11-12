// Generates signatures.json independently of Ledger's Go code, using
// Node's Ed25519 and the `canonicalize` RFC 8785 package (npm i canonicalize@2.1.0).
// Usage: node gen-signatures.js > signatures.json
const crypto = require('crypto');
const canonicalize = require('canonicalize');

function key(byte) {
  const der = Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.alloc(32, byte)]);
  const priv = crypto.createPrivateKey({ key: der, format: 'der', type: 'pkcs8' });
  const pub = crypto.createPublicKey(priv).export({ format: 'der', type: 'spki' }).subarray(12);
  return { priv, pub };
}
const b64 = (b) => Buffer.from(b).toString('base64url');
const sha = (b) => crypto.createHash('sha256').update(b).digest('hex');

const root = key(1);
const fp = sha(root.pub);
const T = '2026-11-05T12:00:00.000Z';
const payloads = [
  ['root_key', { public_key: b64(root.pub), created_at: T }],
  ['intent', { id: 'int_' + 'b'.repeat(26), parent_id: null, description: 'Ünïcödé   "quoted" \\ <tag> 😀 é',
    criteria: [{ id: 'tests', statement: 'Tests pass', verification: { kind: 'judgment', reviewer: 'any' } }],
    source: { kind: 'human', reference: null }, owner: fp, created_at: T }],
  ['check_definition', { name: 'unit', category: 'mechanical', setup: null, command: 'go test ./...', working_directory: '.',
    env_allowlist: ['PATH'], timeout_seconds: 86400, output_limit_bytes: 0 }],
  ['operation_receipt', { operation_id: 'op_' + 'c'.repeat(26), operation: 'intent.create', request_digest: 'a'.repeat(64),
    result: { max: 9007199254740991, min: -9007199254740991, zero: 0, nested: { b: [3, 1, 2], a: '\u0000' } }, members: [], at: T }],
];
const ses = 'ses_' + 'a'.repeat(26);
const sym = (path, name, revision) => ({ path, language: 'typescript', name, kind: 'function', disambiguator: null, revision });
// schema_version 2 (ADR 0027): records bind native store trees.
const payloadsV2 = [
  ['repository', { id: 'rep_' + 'a'.repeat(26), hash_algorithm: 'sha256', case_policy: 'insensitive',
    analyzers: ['python', 'typescript'], created_at: T }],
  ['capability', { id: 'cap_' + 'a'.repeat(26), session_id: ses, issuer: { kind: 'root', fingerprint: fp },
    allow: ['src/**'], deny: [], may_add_dependencies: false, may_advance: ['main', 'release/1.x'],
    expires_at: T, created_at: T }],
  ['change_manifest', { change_id: 'chg_' + 'a'.repeat(26), intent_id: 'int_' + 'b'.repeat(26), workspace_id: 'wsp_' + 'a'.repeat(26),
    author_session: ses, context_refs: [], plan: null, base_tree: 'c'.repeat(64), tree: 'd'.repeat(64),
    files: ['src/a.ts', 'src/Z.ts', 'src/é.ts'],
    symbols_read: [sym('src/a.ts', 'a', '1'.repeat(64))], symbols_written: [sym('src/é.ts', 'ü', '2'.repeat(64))],
    op_head: 'f'.repeat(64),
    // Keys whose RFC 8785 order (UTF-16 code units) differs from insertion order.
    symbol_maps: { 'src/é.ts': '3'.repeat(64), 'src/a.ts': '4'.repeat(64), 'src/Z.ts': '5'.repeat(64) } }],
  ['line_advance', { line: 'main', previous: null, manifest: 'a'.repeat(64), selection: 'b'.repeat(64), actor: fp, at: T }],
];
const seal = (version) => ([kind, payload]) => {
  const unsigned = { schema_version: version, kind, payload, signer: fp };
  const unsignedBytes = canonicalize(unsigned);
  const message = Buffer.concat([Buffer.from(`ledger-record-v${version}\n`), Buffer.from(unsignedBytes)]);
  const signature = b64(crypto.sign(null, message, root.priv));
  const signed = canonicalize({ ...unsigned, signature });
  return { schema_version: version, kind, public_key: b64(root.pub), fingerprint: fp,
    unsigned: unsignedBytes, signature, signed, hash: sha(signed) };
};
const out = [...payloads.map(seal(1)), ...payloadsV2.map(seal(2))];
console.log(JSON.stringify(out, null, 2));
