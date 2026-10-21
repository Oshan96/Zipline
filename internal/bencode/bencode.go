// Package bencode reads and writes bencode, the encoding of torrent files and BitTorrent messages.
package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Bencode, BitTorrent's encoding: integers i42e, strings 4:spam, lists l…e and dictionaries
// d…e with sorted keys. Decoded values are int64, string (raw bytes), []any and map[string]any.

var errInvalid = errors.New("not valid bencode")

type decoder struct {
	b     []byte
	pos   int
	depth int
	spans map[string][2]int // where top-level dictionary values start and end (for the info hash)
}

// Decode decodes one value; trailing bytes are an error.
func Decode(b []byte) (any, error) {
	d := &decoder{b: b}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	if d.pos != len(b) {
		return nil, fmt.Errorf("%w: %d trailing bytes", errInvalid, len(b)-d.pos)
	}
	return v, nil
}

// DecodePrefix decodes one value at the start of b and says how long it was (for messages
// that carry raw data after a bencoded dictionary, like ut_metadata).
func DecodePrefix(b []byte) (any, int, error) {
	d := &decoder{b: b}
	v, err := d.value()
	return v, d.pos, err
}

// DecodeDictSpans decodes a dictionary and also returns the raw bytes of each top-level value.
func DecodeDictSpans(b []byte) (map[string]any, map[string][]byte, error) {
	d := &decoder{b: b, spans: map[string][2]int{}}
	v, err := d.value()
	if err != nil {
		return nil, nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("%w: not a dictionary", errInvalid)
	}
	raw := map[string][]byte{}
	for k, s := range d.spans {
		raw[k] = b[s[0]:s[1]]
	}
	return m, raw, nil
}

func (d *decoder) value() (any, error) {
	if d.pos >= len(d.b) {
		return nil, fmt.Errorf("%w: unexpected end", errInvalid)
	}
	d.depth++
	defer func() { d.depth-- }()
	if d.depth > 64 {
		return nil, fmt.Errorf("%w: nested too deep", errInvalid)
	}
	switch c := d.b[d.pos]; {
	case c == 'i':
		end := bytes.IndexByte(d.b[d.pos:], 'e')
		if end < 0 {
			return nil, fmt.Errorf("%w: unterminated integer", errInvalid)
		}
		s := string(d.b[d.pos+1 : d.pos+end])
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || s == "-0" || len(s) > 1 && (s[0] == '0' || s[:2] == "-0") {
			return nil, fmt.Errorf("%w: bad integer %q", errInvalid, s)
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
				return nil, fmt.Errorf("%w: unterminated list", errInvalid)
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
				return nil, fmt.Errorf("%w: unterminated dictionary", errInvalid)
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
	return nil, fmt.Errorf("%w: unexpected %q", errInvalid, d.b[d.pos])
}

func (d *decoder) str() (string, error) {
	colon := bytes.IndexByte(d.b[d.pos:], ':')
	if colon < 0 || colon > 12 {
		return "", fmt.Errorf("%w: bad string length", errInvalid)
	}
	n, err := strconv.Atoi(string(d.b[d.pos : d.pos+colon]))
	if err != nil || n < 0 {
		return "", fmt.Errorf("%w: bad string length", errInvalid)
	}
	start := d.pos + colon + 1
	if start+n > len(d.b) || start+n < start {
		return "", fmt.Errorf("%w: string runs past the end", errInvalid)
	}
	d.pos = start + n
	return string(d.b[start : start+n]), nil
}

// Encode encodes ints, strings, []byte, lists and string-keyed maps (keys sorted).
func Encode(v any) []byte {
	var b bytes.Buffer
	encodeTo(&b, v)
	return b.Bytes()
}

func encodeTo(b *bytes.Buffer, v any) {
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
			encodeTo(b, e)
		}
		b.WriteByte('e')
	case []string:
		b.WriteByte('l')
		for _, e := range x {
			encodeTo(b, e)
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
			encodeTo(b, k)
			encodeTo(b, x[k])
		}
		b.WriteByte('e')
	case Raw:
		b.Write(x)
	default:
		panic(fmt.Sprintf("bencode: unsupported %T", v))
	}
}

// Raw is already encoded (an info dictionary kept byte for byte).
type Raw []byte

// helpers for reading decoded dictionaries
func Str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func Int(m map[string]any, k string) (int64, bool) {
	n, ok := m[k].(int64)
	return n, ok
}

func Dict(m map[string]any, k string) map[string]any {
	d, _ := m[k].(map[string]any)
	return d
}

func List(m map[string]any, k string) []any {
	l, _ := m[k].([]any)
	return l
}
