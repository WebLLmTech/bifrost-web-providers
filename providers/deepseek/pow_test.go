package deepseek

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

type powVectorsFile struct {
	HashVectors []struct {
		Input  string `json:"input"`
		Digest string `json:"digest"`
	} `json:"hashVectors"`
	SolveVectors []struct {
		Challenge  string `json:"challenge"`
		Prefix     string `json:"prefix"`
		Difficulty int    `json:"difficulty"`
		Answer     int64  `json:"answer"`
	} `json:"solveVectors"`
}

// loadPowVectors reads the vectors generated from the real DeepSeek wasm
// binary (see testdata/vectors.mjs and docs/DEEPSEEK-POW.md).
func loadPowVectors(t *testing.T) powVectorsFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/pow_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v powVectorsFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(v.HashVectors) == 0 || len(v.SolveVectors) == 0 {
		t.Fatal("vector file is empty")
	}
	return v
}

// splitPrefix parses the "<salt>_<expire_at>_" prefix emitted by vectors.mjs.
func splitPrefix(t *testing.T, prefix string) (string, int64) {
	t.Helper()
	salt, expire, ok := strings.Cut(strings.TrimSuffix(prefix, "_"), "_")
	if !ok {
		t.Fatalf("bad prefix %q", prefix)
	}
	n, err := strconv.ParseInt(expire, 10, 64)
	if err != nil {
		t.Fatalf("bad expire_at in %q: %v", prefix, err)
	}
	return salt, n
}

func TestDeepSeekHashV1MatchesWasmVectors(t *testing.T) {
	for _, v := range loadPowVectors(t).HashVectors {
		want, err := hex.DecodeString(v.Digest)
		if err != nil {
			t.Fatalf("bad digest %q: %v", v.Digest, err)
		}
		got := deepseekHashV1([]byte(v.Input))
		if !equalBytes(got[:], want) {
			t.Errorf("hash(%q) = %x, want %s", v.Input, got, v.Digest)
		}
	}
}

func TestSolvePowMatchesWasmVectors(t *testing.T) {
	for _, v := range loadPowVectors(t).SolveVectors {
		salt, expireAt := splitPrefix(t, v.Prefix)
		nonce, ok := solvePow(v.Challenge, salt, expireAt, v.Difficulty, 0)
		if !ok {
			t.Fatalf("challenge %s not solved", v.Challenge)
		}
		if nonce != v.Answer {
			t.Errorf("challenge %s solved with nonce %d, want %d", v.Challenge, nonce, v.Answer)
		}
	}
}

func TestAnswerChallengeEncodesExactKeyOrder(t *testing.T) {
	v := loadPowVectors(t).SolveVectors[0]
	salt, expireAt := splitPrefix(t, v.Prefix)
	encoded, err := answerChallenge(powChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  v.Challenge,
		Salt:       salt,
		Difficulty: v.Difficulty,
		ExpireAt:   expireAt,
		Signature:  "sig-123",
	}, "/api/v0/chat/completion", 0)
	if err != nil {
		t.Fatalf("answerChallenge: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("answer is not standard base64: %v", err)
	}
	want := `{"algorithm":"DeepSeekHashV1","challenge":"` + v.Challenge +
		`","salt":"` + salt + `","answer":` + strconv.FormatInt(v.Answer, 10) +
		`,"signature":"sig-123","target_path":"/api/v0/chat/completion"}`
	if string(payload) != want {
		t.Errorf("payload =\n%s\nwant\n%s", payload, want)
	}
}

func TestAnswerChallengeRejectsUnknownAlgorithm(t *testing.T) {
	_, err := answerChallenge(powChallenge{Algorithm: "Sha256"}, "/api/v0/chat/completion", 0)
	if err == nil || !strings.Contains(err.Error(), "Unsupported algorithm: Sha256") {
		t.Fatalf("expected unsupported algorithm error, got %v", err)
	}
}

func TestSolvePowHonorsIterationCap(t *testing.T) {
	// The all-zero digest is unreachable within a handful of iterations; the
	// cap must stop the scan instead of running to difficulty.
	impossible := strings.Repeat("00", 32)
	if _, ok := solvePow(impossible, "salt", 1, 144000, 8); ok {
		t.Fatal("impossible challenge solved")
	}
}

func TestSolvePowRejectsBadChallenge(t *testing.T) {
	if _, ok := solvePow("not-hex", "salt", 1, 100, 0); ok {
		t.Fatal("non-hex challenge must not solve")
	}
	if _, ok := solvePow(strings.Repeat("ab", 16), "salt", 1, 100, 0); ok {
		t.Fatal("short challenge must not solve")
	}
}
