package main

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Bencode, BitTorrent's encoding: integers i42e, strings 4:spam, lists l…e and dictionaries
// d…e with sorted keys. Decoded values are int64, string (raw bytes), []any and map[string]any.

var errBencode = errors.New("not valid bencode")

type bdecoder struct {
	b     []byte
	pos   int
	depth int
	spans map[string][2]int // where top-level dictionary values start and end (for the info hash)
}

// bdecode decodes one value; trailing bytes are an error.
func bdecode(b []byte) (any, error) {
	d := &bdecoder{b: b}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	if d.pos != len(b) {
		return nil, fmt.Errorf("%w: %d trailing bytes", errBencode, len(b)-d.pos)
	}
	return v, nil
}

// bdecodePrefix decodes one value at the start of b and says how long it was (for messages
// that carry raw data after a bencoded dictionary, like ut_metadata).
func bdecodePrefix(b []byte) (any, int, error) {
	d := &bdecoder{b: b}
	v, err := d.value()
	return v, d.pos, err
}

// bdecodeDictSpans decodes a dictionary and also returns the raw bytes of each top-level value.
func bdecodeDictSpans(b []byte) (map[string]any, map[string][]byte, error) {
	d := &bdecoder{b: b, spans: map[string][2]int{}}
	v, err := d.value()
	if err != nil {
		return nil, nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("%w: not a dictionary", errBencode)
	}
	raw := map[string][]byte{}
	for k, s := range d.spans {
		raw[k] = b[s[0]:s[1]]
	}
	return m, raw, nil
}

func (d *bdecoder) value() (any, error) {
	if d.pos >= len(d.b) {
		return nil, fmt.Errorf("%w: unexpected end", errBencode)
	}
	d.depth++
	defer func() { d.depth-- }()
	if d.depth > 64 {
		return nil, fmt.Errorf("%w: nested too deep", errBencode)
	}
	switch c := d.b[d.pos]; {
	case c == 'i':
		end := bytes.IndexByte(d.b[d.pos:], 'e')
		if end < 0 {
			return nil, fmt.Errorf("%w: unterminated integer", errBencode)
		}
		s := string(d.b[d.pos+1 : d.pos+end])
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || s == "-0" || len(s) > 1 && (s[0] == '0' || s[:2] == "-0") {
			return nil, fmt.Errorf("%w: bad integer %q", errBencode, s)
		}
		d.pos += end + 1
		return n, nil
	case c >= '0' && c <= '9':
		s, err := d.str()
		return s, err
	case c == 'l':
		d.pos++
		list := []any{}
		for {
			if d.pos >= len(d.b) {
				return nil, fmt.Errorf("%w: unterminated list", errBencode)
			}
			if d.b[d.pos] == 'e' {
				d.pos++
				return list, nil
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case c == 'd':
		top := d.depth == 1
		d.pos++
		m := map[string]any{}
		for {
			if d.pos >= len(d.b) {
				return nil, fmt.Errorf("%w: unterminated dictionary", errBencode)
			}
			if d.b[d.pos] == 'e' {
				d.pos++
				return m, nil
			}
			k, err := d.str()
			if err != nil {
				return nil, err
			}
			start := d.pos
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			m[k] = v
			if top && d.spans != nil {
				d.spans[k] = [2]int{start, d.pos}
			}
		}
	}
	return nil, fmt.Errorf("%w: unexpected %q", errBencode, d.b[d.pos])
}

func (d *bdecoder) str() (string, error) {
	colon := bytes.IndexByte(d.b[d.pos:], ':')
	if colon < 0 || colon > 12 {
		return "", fmt.Errorf("%w: bad string length", errBencode)
	}
	n, err := strconv.Atoi(string(d.b[d.pos : d.pos+colon]))
	if err != nil || n < 0 {
		return "", fmt.Errorf("%w: bad string length", errBencode)
	}
	start := d.pos + colon + 1
	if start+n > len(d.b) || start+n < start {
		return "", fmt.Errorf("%w: string runs past the end", errBencode)
	}
	d.pos = start + n
	return string(d.b[start : start+n]), nil
}

// bencode encodes ints, strings, []byte, lists and string-keyed maps (keys sorted).
func bencode(v any) []byte {
	var b bytes.Buffer
	bencodeTo(&b, v)
	return b.Bytes()
}

func bencodeTo(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int:
		fmt.Fprintf(b, "i%de", x)
	case int64:
		fmt.Fprintf(b, "i%de", x)
	case uint16:
		fmt.Fprintf(b, "i%de", x)
	case string:
		fmt.Fprintf(b, "%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(b, "%d:", len(x))
		b.Write(x)
	case []any:
		b.WriteByte('l')
		for _, e := range x {
			bencodeTo(b, e)
		}
		b.WriteByte('e')
	case []string:
		b.WriteByte('l')
		for _, e := range x {
			bencodeTo(b, e)
		}
		b.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('d')
		for _, k := range keys {
			bencodeTo(b, k)
			bencodeTo(b, x[k])
		}
		b.WriteByte('e')
	case bencodedRaw:
		b.Write(x)
	default:
		panic(fmt.Sprintf("bencode: unsupported %T", v))
	}
}

// bencodedRaw is already encoded (an info dictionary kept byte for byte).
type bencodedRaw []byte

// helpers for reading decoded dictionaries
func bstr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func bint(m map[string]any, k string) (int64, bool) {
	n, ok := m[k].(int64)
	return n, ok
}

func bdict(m map[string]any, k string) map[string]any {
	d, _ := m[k].(map[string]any)
	return d
}

func blist(m map[string]any, k string) []any {
	l, _ := m[k].([]any)
	return l
}
