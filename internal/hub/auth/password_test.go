package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash = %q", h)
	}
	if !VerifyPassword(h, "correct horse") {
		t.Fatal("verify must accept the right password")
	}
	if VerifyPassword(h, "wrong") {
		t.Fatal("verify must reject a wrong password")
	}
	if VerifyPassword("garbage", "x") {
		t.Fatal("verify must reject a malformed hash")
	}
	if VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$$", "x") {
		t.Fatal("verify must reject a truncated hash")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("pw")
	b, _ := HashPassword("pw")
	if a == b {
		t.Fatal("two hashes of the same password must differ")
	}
}
