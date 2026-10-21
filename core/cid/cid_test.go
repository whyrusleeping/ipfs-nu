package cid

import (
	"bytes"
	"testing"
)

func TestTextAndBinary(t *testing.T) {
	// From the atproto data-model fixtures (SHA-256 DRISL and raw) and dasl-testing (BLAKE3 raw).
	for _, c := range []struct {
		s     string
		codec Codec
		hash  Hash
	}{
		{"bafyreidfayvfuwqa7qlnopdjiqrxzs6blmoeu4rujcjtnci5beludirz2a", DRISL, SHA256},
		{"bafkreiccldh766hwcnuxnf2wh6jgzepf2nlu2lvcllt63eww5p6chi4ity", Raw, SHA256},
	} {
		v, err := Parse(c.s)
		if err != nil || v.Codec() != c.codec || v.Hash() != c.hash || v.String() != c.s {
			t.Fatalf("%s: %v %v %v %v", c.s, err, v.Codec(), v.Hash(), v)
		}
		w, err := Decode(v.Bytes())
		if err != nil || w != v {
			t.Fatalf("binary round trip: %v", err)
		}
	}
	b3 := New(Raw, Sum(BLAKE3, []byte("hi")))
	if p, err := Parse(b3.String()); err != nil || p != b3 {
		t.Fatalf("blake3 round trip: %v", err)
	}
	// Rejected: uppercase, other multibase, CIDv0, wrong length, non-canonical trailing bits.
	good := "bafyreidfayvfuwqa7qlnopdjiqrxzs6blmoeu4rujcjtnci5beludirz2a"
	for _, bad := range []string{
		"BAFYREIDFAYVFUWQA7QLNOPDJIQRXZS6BLMOEU4RUJCJTNCI5BELUDIRZ2A",
		"zdpuAyvkgEDQm9TenwGkd5eNaosSxjgEYd8QatfPetgB1CdEZ",
		"QmPZ9gcCEpqKTo6aq61g2nXGUhM4iCL3ewB6LDXZCtioEB",
		good[:len(good)-1],
		good[:len(good)-1] + "b",
		"bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi", // dag-pb
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestWholeObjectLimit(t *testing.T) {
	big := make([]byte, MaxWholeObject+1)
	for _, k := range []struct {
		c   Codec
		h   Hash
		err bool
	}{{Raw, SHA256, true}, {DRISL, SHA256, true}, {DRISL, BLAKE3, true}, {Raw, BLAKE3, false}} {
		c, err := SumCID(k.c, k.h, big)
		if (err != nil) != k.err {
			t.Fatalf("%v/%v: err %v", k.c, k.h, err)
		}
		if err == nil && c.Verify(big) != nil {
			t.Fatal("verify failed")
		}
	}
	c, _ := SumCID(Raw, SHA256, []byte("x"))
	if c.Verify([]byte("y")) != ErrMismatch {
		t.Fatal("mismatch not detected")
	}
}

func TestForeign(t *testing.T) {
	dagpb, err := ParseForeign("bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi")
	if err != nil || !IsCIDv1(dagpb) || ForeignString(dagpb) != "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi" {
		t.Fatal(err)
	}
	if IsCIDv1(append(bytes.Clone(dagpb), 0)) || IsCIDv1(dagpb[:10]) {
		t.Fatal("bad lengths accepted")
	}
}
