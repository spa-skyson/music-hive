package auth

import (
	"strings"
	"testing"
)

func TestSubsonicCipherRoundTrip(t *testing.T) {
	c, err := NewSubsonicCipher("secret-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Seal("sesame")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "" || strings.Contains(enc, "sesame") {
		t.Fatalf("ciphertext leaks or empty: %q", enc)
	}
	clear, err := c.Open(enc)
	if err != nil || clear != "sesame" {
		t.Fatalf("open=%q err=%v", clear, err)
	}
	// одинаковый clear — разные блобы (случайный nonce)
	enc2, _ := c.Seal("sesame")
	if enc == enc2 {
		t.Fatal("nonce reuse")
	}
}

func TestSubsonicCipherTamperAndWrongKey(t *testing.T) {
	c1, _ := NewSubsonicCipher("secret-one", nil)
	enc, _ := c1.Seal("sesame")
	// чужой ключ не открывает
	c2, _ := NewSubsonicCipher("secret-two", nil)
	if _, err := c2.Open(enc); err == nil {
		t.Fatal("wrong key opened blob")
	}
	// битый блоб
	if _, err := c1.Open(enc[:len(enc)-2] + "AA"); err == nil {
		t.Fatal("tampered blob opened")
	}
	if _, err := c1.Open("not-base64!!"); err == nil {
		t.Fatal("garbage opened")
	}
}

func TestSubsonicCipherHKDFFromSessionIKM(t *testing.T) {
	// без SUBSONIC_SECRET ключ — HKDF от сессионного IKM с сепаратором
	c1, _ := NewSubsonicCipher("", []byte("session-ikm-0123456789"))
	c2, _ := NewSubsonicCipher("", []byte("session-ikm-0123456789"))
	c3, _ := NewSubsonicCipher("", []byte("other-session-ikm--------"))
	enc, err := c1.Seal("sesame")
	if err != nil {
		t.Fatal(err)
	}
	if clear, err := c2.Open(enc); err != nil || clear != "sesame" {
		t.Fatalf("same ikm: %q %v", clear, err)
	}
	if _, err := c3.Open(enc); err == nil {
		t.Fatal("different ikm opened blob (separation broken)")
	}
}

func TestMD5Hex(t *testing.T) {
	if got := MD5Hex(""); got != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("md5('')=%q", got)
	}
	if got := MD5Hex("sesame"); len(got) != 32 {
		t.Fatalf("md5('sesame')=%q", got)
	}
}
