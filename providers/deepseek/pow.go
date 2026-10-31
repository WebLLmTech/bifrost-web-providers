package deepseek

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
	"strconv"
)

// DeepSeek protects write endpoints with a proof-of-work challenge. The
// original client solves it inside lib/sha3_wasm_bg.7b9ca65ddd.wasm; the
// algorithm was verified against that binary byte-for-byte (see
// docs/DEEPSEEK-POW.md and testdata/verify.mjs): DeepSeekHashV1 is a SHA3-256
// sponge (rate 136, pad domain 0x06, 32-byte output) whose Keccak-f1600
// permutation skips round 0, i.e. it runs rounds 1..23 with the standard
// round constants. wasm_solve then scans nonce in [0, difficulty) for
// hex(hash(salt + "_" + expire_at + "_" + nonce)) == challenge.
// This is a real solver: no cached answers, no skipped challenges.

// keccakRC holds the standard Keccak-f1600 round constants. Round 0 is
// intentionally never applied, matching the DeepSeek wasm build.
var keccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// keccakRho lists rotation offsets indexed by lane x+5y.
var keccakRho = [25]uint{
	0, 1, 62, 28, 27,
	36, 44, 6, 55, 20,
	3, 10, 43, 25, 39,
	41, 45, 15, 21, 8,
	18, 2, 61, 56, 14,
}

// keccakF1600 runs the permutation starting at round 1 (round 0 skipped).
func keccakF1600(a *[25]uint64) {
	var b [25]uint64
	var c [5]uint64
	var d [5]uint64
	for round := 1; round < 24; round++ {
		for x := 0; x < 5; x++ {
			c[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ bits.RotateLeft64(c[(x+1)%5], 1)
		}
		for i := 0; i < 25; i++ {
			a[i] ^= d[i%5]
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				b[y+5*((2*x+3*y)%5)] = bits.RotateLeft64(a[x+5*y], int(keccakRho[x+5*y]))
			}
		}
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				a[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		a[0] ^= keccakRC[round]
	}
}

const (
	spongeRate   = 136
	spongeDomain = 0x06
	digestSize   = 32
)

// deepseekHashV1 is the wasm_deepseek_hash_v1 equivalent.
func deepseekHashV1(data []byte) [digestSize]byte {
	var state [25]uint64
	for len(data) >= spongeRate {
		absorbBlock(&state, data[:spongeRate])
		keccakF1600(&state)
		data = data[spongeRate:]
	}
	var block [spongeRate]byte
	copy(block[:], data)
	block[len(data)] = spongeDomain
	block[spongeRate-1] |= 0x80
	absorbBlock(&state, block[:])
	keccakF1600(&state)
	var out [digestSize]byte
	for i := 0; i < digestSize/8; i++ {
		lePutUint64(out[i*8:], state[i])
	}
	return out
}

func absorbBlock(state *[25]uint64, block []byte) {
	for i := 0; i < spongeRate/8; i++ {
		state[i] ^= leUint64(block[i*8:])
	}
}

func leUint64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func lePutUint64(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

// powChallenge is the server-issued challenge from create_pow_challenge.
type powChallenge struct {
	Algorithm string `json:"algorithm"`
	Challenge string `json:"challenge"`
	Salt      string `json:"salt"`
	Difficulty int   `json:"difficulty"`
	ExpireAt  int64  `json:"expire_at"`
	Signature string `json:"signature"`
}

// powAnswer mirrors btoa(JSON.stringify({...})) from lib/challenge.ts,
// including the exact key order expected by the server.
type powAnswer struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Answer     int64  `json:"answer"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// solvePow scans nonce in [0, difficulty) like wasm_solve. maxIterations
// bounds CPU use; production difficulty is 144000.
func solvePow(challengeHex, salt string, expireAt int64, difficulty, maxIterations int) (int64, bool) {
	if difficulty < 0 {
		difficulty = 0
	}
	limit := difficulty
	if maxIterations > 0 && limit > maxIterations {
		limit = maxIterations
	}
	prefix := salt + "_" + strconv.FormatInt(expireAt, 10) + "_"
	target, err := hex.DecodeString(challengeHex)
	if err != nil || len(target) != digestSize {
		return 0, false
	}
	buf := make([]byte, 0, len(prefix)+24)
	for nonce := 0; nonce < limit; nonce++ {
		buf = append(buf[:0], prefix...)
		buf = strconv.AppendInt(buf, int64(nonce), 10)
		digest := deepseekHashV1(buf)
		if equalBytes(digest[:], target) {
			return int64(nonce), true
		}
	}
	return 0, false
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// answerChallenge solves the challenge and encodes the X-Ds-Pow-Response
// payload exactly like answerChallenge() in lib/challenge.ts.
func answerChallenge(ch powChallenge, targetPath string, maxIterations int) (string, error) {
	if ch.Algorithm != "DeepSeekHashV1" {
		return "", fmt.Errorf("Unsupported algorithm: %s", ch.Algorithm)
	}
	nonce, ok := solvePow(ch.Challenge, ch.Salt, ch.ExpireAt, ch.Difficulty, maxIterations)
	if !ok {
		return "", fmt.Errorf("get challenge failed")
	}
	payload, err := json.Marshal(powAnswer{
		Algorithm:  ch.Algorithm,
		Challenge:  ch.Challenge,
		Salt:       ch.Salt,
		Answer:     nonce,
		Signature:  ch.Signature,
		TargetPath: targetPath,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(payload), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("cryptographic random source unavailable")
	}
	return hex.EncodeToString(b)
}

// uuidV4 returns a dashed random UUID for synthetic telemetry identifiers.
func uuidV4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptographic random source unavailable")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func uuidHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptographic random source unavailable")
	}
	return hex.EncodeToString(b)
}
