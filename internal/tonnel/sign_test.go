package tonnel

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// The reference values come from OpenSSL, which is the scheme CryptoJS
// implements on the Tonnel backend:
//
//	printf '1700000000' | openssl enc -aes-256-cbc -md md5 \
//	    -S 0102030405060708 -pass pass:yowtfisthispieceofshitiiit -base64 -A
//
// If this test ever fails, every write endpoint (buy, list, cancel) will be
// rejected — the signature is the only thing the server checks besides authData.
func TestEVPBytesToKeyMatchesOpenSSL(t *testing.T) {
	salt, err := hex.DecodeString("0102030405060708")
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}

	key, iv := evpBytesToKey([]byte(signPassword), salt, 32, 16)

	const wantKey = "3c0f2b7be56df9f8157fb9949ac6cd063bb67aa24bc72acbc3998bddf789d12e"
	const wantIV = "a3d79871a6e2a60127048b1203534cc0"

	if got := hex.EncodeToString(key); got != wantKey {
		t.Errorf("key = %s, want %s", got, wantKey)
	}
	if got := hex.EncodeToString(iv); got != wantIV {
		t.Errorf("iv = %s, want %s", got, wantIV)
	}
}

func TestSignWithSaltMatchesOpenSSL(t *testing.T) {
	salt, _ := hex.DecodeString("0102030405060708")

	got, err := signWithSalt("1700000000", salt)
	if err != nil {
		t.Fatalf("signWithSalt: %v", err)
	}
	const want = "U2FsdGVkX18BAgMEBQYHCPJFfP2723sEMreVy3JHzxM="
	if got != want {
		t.Errorf("wtf = %q, want %q", got, want)
	}
}

func TestSignRoundTrip(t *testing.T) {
	now := time.Unix(1712345678, 0)

	ts, wtf, err := Sign(now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if ts != "1712345678" {
		t.Errorf("timestamp = %q, want 1712345678", ts)
	}
	if !strings.HasPrefix(wtf, "U2FsdGVkX1") {
		t.Errorf("wtf %q does not start with the Salted__ marker", wtf)
	}

	back, err := VerifySignature(wtf)
	if err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
	if back != ts {
		t.Errorf("decoded %q, want %q", back, ts)
	}
}

func TestSignUsesFreshSalt(t *testing.T) {
	// A constant salt would make every signature identical, which is exactly
	// the pattern an anti-bot layer looks for.
	_, a, err := Sign(time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	_, b, err := Sign(time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if a == b {
		t.Error("two signatures of the same timestamp are identical; the salt is not random")
	}
}

func TestVerifySignatureRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not base64!!", "aGVsbG8="} {
		if _, err := VerifySignature(in); err == nil {
			t.Errorf("VerifySignature(%q) accepted invalid input", in)
		}
	}
}

func TestPKCS7RoundTrip(t *testing.T) {
	for _, in := range []string{"", "a", "0123456789abcde", "0123456789abcdef", "0123456789abcdefg"} {
		padded := pkcs7Pad([]byte(in), 16)
		if len(padded)%16 != 0 {
			t.Fatalf("padded %q to %d bytes, not a block multiple", in, len(padded))
		}
		out, err := pkcs7Unpad(padded, 16)
		if err != nil {
			t.Fatalf("unpad %q: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("round trip of %q gave %q", in, out)
		}
	}
}
