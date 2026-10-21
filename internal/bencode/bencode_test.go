package bencode

import (
	"fmt"
	"strings"
	"testing"
)

func TestBencode(t *testing.T) {
	in := map[string]any{"b": []any{int64(1), "x", int64(-3)}, "a": "spam", "c": map[string]any{"z": int64(0)}}
	enc := Encode(in)
	if string(enc) != "d1:a4:spam1:bli1e1:xi-3ee1:cd1:zi0eee" {
		t.Fatalf("encode: %s", enc)
	}
	out, err := Decode(enc)
	if err != nil || fmt.Sprint(out) != fmt.Sprint(in) {
		t.Fatalf("decode: %v %v", out, err)
	}
	for _, bad := range []string{"i01e", "i-0e", "4:abc", "l", "d1:ai1e", "i1ex", "3:abcd", "di1ei2ee", strings.Repeat("l", 100) + strings.Repeat("e", 100)} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	v, n, err := DecodePrefix([]byte("d8:msg_typei1e5:piecei0eeRAWDATA"))
	if err != nil || n != 25 || bint2(v, "msg_type") != 1 {
		t.Fatalf("prefix: %v %d %v", v, n, err)
	}
}

func bint2(v any, k string) int64 {
	m, _ := v.(map[string]any)
	n, _ := Int(m, k)
	return n
}
