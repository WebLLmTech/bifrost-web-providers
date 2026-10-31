// Verify DeepSeekHashV1 == SHA3-256 with Keccak-f round 0 skipped, against the
// original wasm, and generate cross-check vectors for the Go implementation.
import { readFileSync } from "node:fs";

const bytes = readFileSync(process.argv[2]);
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


const vectors = JSON.parse(readFileSync(new URL("./pow_vectors.json", import.meta.url)));
let count = 0;
for (const v of vectors.hashVectors) {
  if (wasmHash(v.input) !== v.digest) throw new Error("hash mismatch");
  count++;
}
for (const v of vectors.solveVectors) {
  const got = wasmSolve(v.challenge, v.prefix, v.difficulty);
  if (got.status !== 1 || got.value !== v.answer) throw new Error("solve mismatch: " + JSON.stringify({v,got}));
  count++;
}
console.log("WASM verified:", count, "vectors");
