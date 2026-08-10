package tonnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"time"
)

// signPassword is the fixed passphrase the Tonnel backend uses to validate the
// `wtf` field on every write request. All the field actually carries is an
// encrypted unix timestamp, which the server decrypts and compares against the
// plaintext `timestamp` field sent alongside it.
const signPassword = "yowtfisthispieceofshitiiit"

// saltedMagic is the OpenSSL "Salted__" header that CryptoJS emits.
var saltedMagic = []byte("Salted__")

// evpBytesToKey reproduces OpenSSL's EVP_BytesToKey with MD5 and a single
// iteration, which is what CryptoJS uses to derive a key/IV pair from a
// passphrase and salt.
func evpBytesToKey(password, salt []byte, keyLen, ivLen int) (key, iv []byte) {
	var out []byte
	var prev []byte
	for len(out) < keyLen+ivLen {
		h := md5.New()
		h.Write(prev)
		h.Write(password)
		h.Write(salt)
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen], out[keyLen : keyLen+ivLen]
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	pad := make([]byte, n)
	for i := range pad {
		pad[i] = byte(n)
	}
	return append(b, pad...)
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, errors.New("pkcs7: bad length")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > blockSize || n > len(b) {
		return nil, errors.New("pkcs7: bad padding")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("pkcs7: bad padding")
		}
	}
	return b[:len(b)-n], nil
}

// signWithSalt is the deterministic core of Sign, split out so tests can pin the salt.
func signWithSalt(timestamp string, salt []byte) (string, error) {
	if len(salt) != 8 {
		return "", errors.New("salt must be 8 bytes")
	}
	key, iv := evpBytesToKey([]byte(signPassword), salt, 32, aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pt := pkcs7Pad([]byte(timestamp), aes.BlockSize)
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)

	buf := make([]byte, 0, len(saltedMagic)+len(salt)+len(ct))
	buf = append(buf, saltedMagic...)
	buf = append(buf, salt...)
	buf = append(buf, ct...)
	return base64.StdEncoding.EncodeToString(buf), nil
}

// Sign produces the (timestamp, wtf) pair required by every Tonnel write endpoint.
func Sign(now time.Time) (timestamp, wtf string, err error) {
	timestamp = strconv.FormatInt(now.Unix(), 10)
	salt := make([]byte, 8)
	if _, err = rand.Read(salt); err != nil {
		return "", "", err
	}
	wtf, err = signWithSalt(timestamp, salt)
	return timestamp, wtf, err
}

// VerifySignature decrypts a wtf blob back to its timestamp. Used by the tests
// and the smoke command to prove the derivation matches the reference scheme
// before a real write request depends on it.
func VerifySignature(wtf string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(wtf)
	if err != nil {
		return "", err
	}
	if len(raw) < len(saltedMagic)+8 || string(raw[:len(saltedMagic)]) != string(saltedMagic) {
		return "", errors.New("missing Salted__ header")
	}
	salt := raw[len(saltedMagic) : len(saltedMagic)+8]
	ct := raw[len(saltedMagic)+8:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errors.New("bad ciphertext length")
	}
	key, iv := evpBytesToKey([]byte(signPassword), salt, 32, aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	out, err := pkcs7Unpad(pt, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
