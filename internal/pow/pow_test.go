package pow

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
)

// Vectors verified against DeepSeek's official sha3_wasm_bg.7b9ca65ddd.wasm:
// the WASM solver returns n for challenge Hash("saltabc_1700000000_<n>").
var wasmVectors = []struct {
	n    int
	hash string
}{
	{0, "70fc19133dc93c72dabd4cfaa15dc2ab66bab58886e1b0bddedcab03f8157ccb"},
	{7, "acffb41e9614932eaa16097a191c6e88be549fcfcd71dffa8daff6c8103509f2"},
	{1234, "19a8260bde17ad2042b572d7c4defd5d7eff8f8ce943d581ca9780cd0e98a88e"},
	{99999, "dc95e6d5aec8f00fbbf1a505b6948769173c8d6228bbfdfa43edad778a894791"},
}

func TestHashMatchesOfficialWasm(t *testing.T) {
	for _, v := range wasmVectors {
		h := Hash([]byte(fmt.Sprintf("saltabc_1700000000_%d", v.n)))
		if got := hex.EncodeToString(h[:]); got != v.hash {
			t.Errorf("n=%d: got %s want %s", v.n, got, v.hash)
		}
	}
}

func TestHashMultiBlockInput(t *testing.T) {
	// Inputs longer than the 136-byte rate must absorb several blocks.
	a := Hash(make([]byte, 300))
	b := Hash(make([]byte, 301))
	if a == b {
		t.Fatal("different inputs hashed equal")
	}
}

func TestSolveFindsNonceIncludingZero(t *testing.T) {
	for _, v := range wasmVectors {
		ch := Challenge{Algorithm: SupportedAlgorithm, Challenge: v.hash, Salt: "saltabc", ExpireAt: 1700000000, Difficulty: 144000}
		got, err := Solve(ch)
		if err != nil || got != int64(v.n) {
			t.Errorf("Solve = %d, %v; want %d", got, err, v.n)
		}
	}
}

func TestSolveRejectsBadInput(t *testing.T) {
	if _, err := Solve(Challenge{Algorithm: "Other", Challenge: wasmVectors[0].hash, Difficulty: 10}); err == nil {
		t.Error("expected unsupported algorithm error")
	}
	if _, err := Solve(Challenge{Challenge: "zz", Difficulty: 10}); err == nil {
		t.Error("expected invalid challenge error")
	}
	if _, err := Solve(Challenge{Challenge: wasmVectors[3].hash, Salt: "saltabc", ExpireAt: 1700000000, Difficulty: 10}); err != ErrNoSolution {
		t.Errorf("expected ErrNoSolution, got %v", err)
	}
}

func TestHeaderEncodesResponse(t *testing.T) {
	ch := Challenge{Algorithm: SupportedAlgorithm, Challenge: "c", Salt: "s", Signature: "sig", TargetPath: "/api/v0/chat/completion"}
	raw, err := base64.StdEncoding.DecodeString(Header(ch, 42))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["answer"] != float64(42) || m["signature"] != "sig" || m["target_path"] != "/api/v0/chat/completion" {
		t.Errorf("unexpected header payload %v", m)
	}
}

func BenchmarkSolve(b *testing.B) {
	ch := Challenge{Challenge: wasmVectors[3].hash, Salt: "saltabc", ExpireAt: 1700000000, Difficulty: 144000}
	for i := 0; i < b.N; i++ {
		if _, err := Solve(ch); err != nil {
			b.Fatal(err)
		}
	}
}
