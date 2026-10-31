// Verify DeepSeekHashV1 == SHA3-256 with Keccak-f round 0 skipped, against the
// original wasm, and generate cross-check vectors for the Go implementation.
import { readFileSync, writeFileSync } from "node:fs";

const bytes = readFileSync("/Volumes/DevData/Projects/chat/deepseek-deno-api/lib/sha3_wasm_bg.7b9ca65ddd.wasm");
const { instance } = await WebAssembly.instantiate(bytes, { wbg: {} });
const ex = instance.exports;

function encode(text) {
  const data = typeof text === "string" ? new TextEncoder().encode(text) : text;
  const ptr = ex.__wbindgen_export_0(data.length, 1) >>> 0;
  new Uint8Array(ex.memory.buffer).subarray(ptr, ptr + data.length).set(data);
  return { ptr, len: data.length };
}
function wasmHash(input) {
  const retptr = ex.__wbindgen_add_to_stack_pointer(-16);
  const a = encode(input);
  ex.wasm_deepseek_hash_v1(retptr, a.ptr, a.len);
  const dv = new DataView(ex.memory.buffer);
  const p = dv.getInt32(retptr, true) >>> 0;
  const l = dv.getInt32(retptr + 4, true) >>> 0;
  const s = new TextDecoder().decode(new Uint8Array(ex.memory.buffer).subarray(p, p + l));
  ex.__wbindgen_add_to_stack_pointer(16);
  return s;
}
function wasmSolve(challenge, prefix, difficulty) {
  const retptr = ex.__wbindgen_add_to_stack_pointer(-16);
  const a = encode(challenge);
  const b = encode(prefix);
  ex.wasm_solve(retptr, a.ptr, a.len, b.ptr, b.len, difficulty);
  const dv = new DataView(ex.memory.buffer);
  const status = dv.getInt32(retptr, true);
  const value = dv.getFloat64(retptr + 8, true);
  ex.__wbindgen_add_to_stack_pointer(16);
  return { status, value };
}

// ---- pure JS keccak-f1600 with configurable first round ----
const RC = [
  0x0000000000000001n, 0x0000000000008082n, 0x800000000000808an, 0x8000000080008000n,
  0x000000000000808bn, 0x0000000080000001n, 0x8000000080008081n, 0x8000000000008009n,
  0x000000000000008an, 0x0000000000000088n, 0x0000000080008009n, 0x000000008000000an,
  0x000000008000808bn, 0x800000000000008bn, 0x8000000000008089n, 0x8000000000008003n,
  0x8000000000008002n, 0x8000000000000080n, 0x000000000000800an, 0x800000008000000an,
  0x8000000080008081n, 0x8000000000008080n, 0x0000000080000001n, 0x8000000080008008n,
];
const ROT = [
  [0n, 36n, 3n, 41n, 18n],
  [1n, 44n, 10n, 45n, 2n],
  [62n, 6n, 43n, 15n, 61n],
  [28n, 55n, 25n, 21n, 56n],
  [27n, 20n, 39n, 8n, 14n],
];
const MASK = (1n << 64n) - 1n;
function rotl(x, n) { return ((x << n) | (x >> (64n - n))) & MASK; }

function keccakF(A, firstRound) {
  for (let round = firstRound; round < 24; round++) {
    // theta
    const C = new Array(5);
    for (let x = 0; x < 5; x++) C[x] = A[x][0] ^ A[x][1] ^ A[x][2] ^ A[x][3] ^ A[x][4];
    const D = new Array(5);
    for (let x = 0; x < 5; x++) D[x] = C[(x + 4) % 5] ^ rotl(C[(x + 1) % 5], 1n);
    for (let x = 0; x < 5; x++) for (let y = 0; y < 5; y++) A[x][y] ^= D[x];
    // rho + pi
    const B = Array.from({ length: 5 }, () => new Array(5).fill(0n));
    for (let x = 0; x < 5; x++) for (let y = 0; y < 5; y++) {
      B[y][(2 * x + 3 * y) % 5] = rotl(A[x][y], ROT[x][y]);
    }
    // chi
    for (let x = 0; x < 5; x++) for (let y = 0; y < 5; y++) {
      A[x][y] = B[x][y] ^ ((~B[(x + 1) % 5][y] & MASK) & B[(x + 2) % 5][y]);
    }
    // iota
    A[0][0] ^= RC[round];
  }
}

function sha3Variant(input, { rate = 136, domain = 0x06, firstRound = 0 }) {
  const A = Array.from({ length: 5 }, () => new Array(5).fill(0n));
  const buf = new Uint8Array(rate);
  let pos = 0;
  const absorb = (data) => {
    for (const byte of data) {
      buf[pos++] = byte;
      if (pos === rate) { squeezeBlock(buf); pos = 0; }
    }
  };
  const squeezeBlock = (block) => {
    for (let i = 0; i < rate / 8; i++) {
      let v = 0n;
      for (let j = 0; j < 8; j++) v |= BigInt(block[i * 8 + j]) << BigInt(8 * j);
      const x = i % 5, y = Math.floor(i / 5);
      A[x][y] ^= v;
    }
    keccakF(A, firstRound);
  };
  absorb(input);
  // padding
  const pad = new Uint8Array(rate - pos);
  pad[0] = domain;
  pad[pad.length - 1] |= 0x80;
  absorb(pad);
  // squeeze 32 bytes
  const out = new Uint8Array(32);
  for (let i = 0; i < 4; i++) {
    const v = A[i % 5][Math.floor(i / 5)];
    for (let j = 0; j < 8; j++) out[i * 8 + j] = Number((v >> BigInt(8 * j)) & 0xffn);
  }
  return [...out].map((b) => b.toString(16).padStart(2, "0")).join("");
}

const probeInputs = ["", "abc", "a", "The quick brown fox jumps over the lazy dog"];
for (const input of probeInputs) {
  const want = wasmHash(input);
  const candidates = {
    sha3_256: sha3Variant(new TextEncoder().encode(input), { firstRound: 0 }),
    sha3_skip_round0: sha3Variant(new TextEncoder().encode(input), { firstRound: 1 }),
  };
  const match = Object.entries(candidates).filter(([, v]) => v === want).map(([k]) => k);
  console.log(JSON.stringify(input), "wasm:", want, "match:", match.join(",") || "NONE");
}
