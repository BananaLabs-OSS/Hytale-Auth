package main

import "testing"

func TestSameToken(t *testing.T) {
	if !sameToken("secret", "secret") {
		t.Fatal("equal tokens did not match")
	}
	if sameToken("secret", "other") || sameToken("", "secret") {
		t.Fatal("different tokens matched")
	}
}
