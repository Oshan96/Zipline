package main

import "hash"

// QuickXorHash is OneDrive's whole-file checksum (the "quickXorHash" in an item's details): each
// byte is XORed into a 160-bit ring, 11 bits further along than the one before, and the file's
// length is XORed into the last 8 bytes. This follows Microsoft's published C# reference
// (learn.microsoft.com/onedrive/developer/code-snippets/quickxorhash).

const (
	qxWidth = 160 // bits in the ring
	qxShift = 11  // bits each byte moves along
	qxLast  = 32  // bits used in the ring's last 64-bit cell
)

type quickXor struct {
	data  [3]uint64 // the ring: 64 + 64 + 32 bits
	shift int       // where the next byte goes (bits)
	n     int64
}

func newQuickXor() hash.Hash { return &quickXor{} }

func (q *quickXor) Size() int      { return qxWidth / 8 }
func (q *quickXor) BlockSize() int { return 64 }
func (q *quickXor) Reset()         { *q = quickXor{} }

func (q *quickXor) Write(p []byte) (int, error) {
	cell, off := q.shift/64, q.shift%64
	for i := 0; i < min(len(p), qxWidth); i++ {
		last := cell == len(q.data)-1
		bits := 64
		if last {
			bits = qxLast
		}
		// every 160th byte lands on the same bits: XOR them together first
		var x byte
		for j := i; j < len(p); j += qxWidth {
			x ^= p[j]
		}
		if off <= bits-8 {
			q.data[cell] ^= uint64(x) << off
		} else { // the byte straddles two cells (the last one wraps round to the first)
			next := cell + 1
			if last {
				next = 0
			}
			q.data[cell] ^= uint64(x) << off
			q.data[next] ^= uint64(x) >> (bits - off)
		}
		if off += qxShift; off >= bits { // (11 bits never cross more than one cell)
			if cell++; last {
				cell = 0
			}
			off -= bits
		}
	}
	q.shift = (q.shift + qxShift*(len(p)%qxWidth)) % qxWidth
	q.n += int64(len(p))
	return len(p), nil
}

func (q *quickXor) Sum(b []byte) []byte {
	out := make([]byte, qxWidth/8)
	for i, c := range q.data {
		for k := 0; k < 8 && i*8+k < len(out); k++ {
			out[i*8+k] = byte(c >> (8 * k)) // little-endian, as BitConverter writes it
		}
	}
	for k := 0; k < 8; k++ { // the length, little-endian, over the last 8 bytes
		out[len(out)-8+k] ^= byte(q.n >> (8 * k))
	}
	return append(b, out...)
}
