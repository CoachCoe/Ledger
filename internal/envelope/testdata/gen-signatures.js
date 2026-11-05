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
const out = payloads.map(([kind, payload]) => {
  const unsigned = { schema_version: 1, kind, payload, signer: fp };
  const unsignedBytes = canonicalize(unsigned);
  const message = Buffer.concat([Buffer.from('ledger-record-v1\n'), Buffer.from(unsignedBytes)]);
  const signature = b64(crypto.sign(null, message, root.priv));
  const signed = canonicalize({ ...unsigned, signature });
  return { kind, public_key: b64(root.pub), fingerprint: fp, unsigned: unsignedBytes, signature, signed, hash: sha(signed) };
});
console.log(JSON.stringify(out, null, 2));
