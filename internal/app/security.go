package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead}, nil
}

func (v *Vault) Seal(plain []byte, purpose string) []byte {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return v.aead.Seal(nonce, nonce, plain, []byte("lifeblood:v1:"+purpose))
}

func (v *Vault) Open(encrypted []byte, purpose string) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(encrypted) < n {
		return nil, errors.New("invalid encrypted value")
	}
	return v.aead.Open(nil, encrypted[:n], encrypted[n:], []byte("lifeblood:v1:"+purpose))
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func digest(s string) []byte { d := sha256.Sum256([]byte(s)); return d[:] }

// Oura signs the timestamp header followed by the exact request body. Accept
// its documented seconds timestamp, and millisecond timestamps defensively.
// A two-hour window accommodates the documented hour of delivery retries.
func VerifyWebhook(secret, timestamp, signature string, body []byte, now time.Time) bool {
	if len(timestamp) > 16 {
		return false
	}
	n, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	t := time.Unix(n, 0)
	if len(timestamp) == 13 {
		t = time.UnixMilli(n)
	}
	if t.Before(now.Add(-2*time.Hour)) || t.After(now.Add(5*time.Minute)) {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
