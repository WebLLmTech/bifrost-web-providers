// Verify wasm_solve semantics (hash(prefix + decimal(nonce)) == challenge,
// nonce in [0, difficulty)) and emit cross-check vectors for the Go port.
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

// 1) solve semantics: build a challenge with a known answer.
{
  const prefix = "ffc6f5a1b2c3d4e5f607_1780227446451_";
  const answer = 4242;
  const challenge = wasmHash(prefix + String(answer));
  const r = wasmSolve(challenge, prefix, 144000);
  console.log("solve known-answer:", JSON.stringify({ challenge, r, expected: answer }));
  // not found within a too-small difficulty
  const r2 = wasmSolve(challenge, prefix, 10);
  console.log("solve below range (expect status 0):", JSON.stringify(r2));
  // wrong prefix must not solve
  const r3 = wasmSolve(challenge, "other_1_", 144000);
  console.log("solve wrong prefix (expect status 0):", JSON.stringify(r3));
}

// 2) hash vectors across sponge block boundaries.
const hashVectors = [];
const lengths = [0, 1, 2, 71, 134, 135, 136, 137, 200, 271, 272, 273, 1000];
for (const len of lengths) {
  const input = Array.from({ length: len }, (_, i) => "0123456789abcdef"[i % 16]).join("");
  hashVectors.push({ input, digest: wasmHash(input) });
}
// binary-ish inputs with multibyte UTF-8
for (const input of ["abc", "", "元宝yuanbao🚀", "salt_1780227446451_123456"]) {
  hashVectors.push({ input, digest: wasmHash(input) });
}

// 3) solve vectors with random salts/answers.
const solveVectors = [];
for (let i = 0; i < 5; i++) {
  const salt = Array.from({ length: 20 }, () => "0123456789abcdef"[Math.floor(Math.random() * 16)]).join("");
  const expire = 1780227446451 + i;
  const prefix = `${salt}_${expire}_`;
  const answer = Math.floor(Math.random() * 5000);
  const challenge = wasmHash(prefix + String(answer));
  const r = wasmSolve(challenge, prefix, 144000);
  if (r.status !== 1 || r.value !== answer) throw new Error("unexpected solve result " + JSON.stringify(r));
  solveVectors.push({ challenge, prefix, difficulty: 144000, answer });
}

writeFileSync(
  new URL("./vectors.json", import.meta.url),
  JSON.stringify({ hashVectors, solveVectors }, null, 2),
);
console.log("vectors written:", hashVectors.length, "hash,", solveVectors.length, "solve");
