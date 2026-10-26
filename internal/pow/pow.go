// Package pow implements DeepSeek Web's proof-of-work ("DeepSeekHashV1").
//
// The browser ships the solver as a WASM module (sha3_wasm_bg.*.wasm). The
// algorithm is a SHA3-256 sponge whose Keccak-f[1600] permutation skips the
// first round (23 rounds, constants RC[1..23]). Solving natively removes the
// WASM download, the WASM runtime dependency and per-request instantiation.
package pow

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
)

// Challenge is the data.biz_data.challenge object from create_pow_challenge.
type Challenge struct {
	Algorithm   string `json:"algorithm"`
	Challenge   string `json:"challenge"`
	Salt        string `json:"salt"`
	Signature   string `json:"signature"`
	Difficulty  int64  `json:"difficulty"`
	ExpireAt    int64  `json:"expire_at"`
	ExpireAfter int64  `json:"expire_after,omitempty"`
	TargetPath  string `json:"target_path"`
}

// SupportedAlgorithm is the only algorithm the solver understands.
const SupportedAlgorithm = "DeepSeekHashV1"

var roundConstants = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

var (
	rhoOffsets = [24]int{1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44}
	piLanes    = [24]int{10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1}
)

// keccakF23 is Keccak-f[1600] starting at round 1 instead of round 0.
func keccakF23(st *[25]uint64) {
	var bc [5]uint64
	for round := 1; round < 24; round++ {
		// theta
		for i := 0; i < 5; i++ {
			bc[i] = st[i] ^ st[i+5] ^ st[i+10] ^ st[i+15] ^ st[i+20]
		}
		for i := 0; i < 5; i++ {
			t := bc[(i+4)%5] ^ bits.RotateLeft64(bc[(i+1)%5], 1)
			st[i] ^= t
			st[i+5] ^= t
			st[i+10] ^= t
			st[i+15] ^= t
			st[i+20] ^= t
		}
		// rho + pi
		t := st[1]
		for i := 0; i < 24; i++ {
			j := piLanes[i]
			t, st[j] = st[j], bits.RotateLeft64(t, rhoOffsets[i])
		}
		// chi
		for j := 0; j < 25; j += 5 {
			b0, b1, b2, b3, b4 := st[j], st[j+1], st[j+2], st[j+3], st[j+4]
			st[j] = b0 ^ (^b1 & b2)
			st[j+1] = b1 ^ (^b2 & b3)
			st[j+2] = b2 ^ (^b3 & b4)
			st[j+3] = b3 ^ (^b4 & b0)
			st[j+4] = b4 ^ (^b0 & b1)
		}
		// iota
		st[0] ^= roundConstants[round]
	}
}

const rate = 136 // SHA3-256

// Hash returns DeepSeekHashV1(data).
func Hash(data []byte) [32]byte {
	var st [25]uint64
	for len(data) >= rate {
		absorb(&st, data[:rate])
		data = data[rate:]
	}
	var last [rate]byte
	copy(last[:], data)
	last[len(data)] ^= 0x06
	last[rate-1] ^= 0x80
	absorb(&st, last[:])
	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], st[i])
	}
	return out
}

func absorb(st *[25]uint64, block []byte) {
	for i := 0; i < rate/8; i++ {
		st[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF23(st)
}

// ErrNoSolution is returned when no nonce below the difficulty matches.
var ErrNoSolution = errors.New("pow: no solution found below difficulty")

// Solve finds the nonce n in [0, difficulty) such that
// Hash(salt + "_" + expire_at + "_" + n) == challenge.
func Solve(c Challenge) (int64, error) {
	if c.Algorithm != "" && c.Algorithm != SupportedAlgorithm {
		return 0, fmt.Errorf("pow: unsupported algorithm %q", c.Algorithm)
	}
	target, err := hex.DecodeString(c.Challenge)
	if err != nil || len(target) != 32 {
		return 0, fmt.Errorf("pow: invalid challenge %q", c.Challenge)
	}
	prefix := c.Salt + "_" + strconv.FormatInt(c.ExpireAt, 10) + "_"
	buf := make([]byte, 0, len(prefix)+20)
	buf = append(buf, prefix...)
	for n := int64(0); n < c.Difficulty; n++ {
		h := Hash(strconv.AppendInt(buf, n, 10))
		if string(h[:]) == string(target) {
			return n, nil
		}
	}
	return 0, ErrNoSolution
}

// Header builds the base64 X-DS-PoW-Response header value.
func Header(c Challenge, answer int64) string {
	raw, _ := json.Marshal(map[string]any{
		"algorithm":   c.Algorithm,
		"challenge":   c.Challenge,
		"salt":        c.Salt,
		"answer":      answer,
		"signature":   c.Signature,
		"target_path": c.TargetPath,
	})
	return base64.StdEncoding.EncodeToString(raw)
}
