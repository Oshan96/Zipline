// Package qr draws QR codes, for pairing a phone.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// A small QR code encoder (byte mode, error correction level M, versions 1–10, up to 213
// bytes) for the phone pairing code. It follows ISO/IEC 18004.

type qrBlocks struct {
	ecPerBlock int
	groups     [][2]int // {blocks, data codewords per block}
}

var qrTable = [11]qrBlocks{
	{}, // no version 0
	{10, [][2]int{{1, 16}}},
	{16, [][2]int{{1, 28}}},
	{26, [][2]int{{1, 44}}},
	{18, [][2]int{{2, 32}}},
	{24, [][2]int{{2, 43}}},
	{16, [][2]int{{4, 27}}},
	{18, [][2]int{{4, 31}}},
	{22, [][2]int{{2, 38}, {2, 39}}},
	{22, [][2]int{{3, 36}, {2, 37}}},
	{26, [][2]int{{4, 43}, {1, 44}}},
}

var qrAlign = [11][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}

func (b qrBlocks) dataCodewords() int {
	n := 0
	for _, g := range b.groups {
		n += g[0] * g[1]
	}
	return n
}

// Code is a finished code: Size×Size modules, true = dark.
type Code struct {
	Size    int
	modules [][]bool
	fn      [][]bool // function patterns (not data)
}

func (q *Code) Dark(x, y int) bool { return q.modules[y][x] }

// Encode makes a QR code holding text.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 10; v++ {
		countBits := 8
		if v >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= qrTable[v].dataCodewords()*8 {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errors.New("too long for a QR code")
	}
	// the bit stream: mode, length, bytes, terminator, padding
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>i&1 == 1)
		}
	}
	put(4, 4)
	if ver >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, c := range data {
		put(int(c), 8)
	}
	capBits := qrTable[ver].dataCodewords() * 8
	put(0, min(4, capBits-len(bits)))
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	cw := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			cw[i/8] |= 0x80 >> (i % 8)
		}
	}
	// split into blocks, add error correction, interleave
	t := qrTable[ver]
	var blocks, ecs [][]byte
	off := 0
	for _, g := range t.groups {
		for k := 0; k < g[0]; k++ {
			b := cw[off : off+g[1]]
			off += g[1]
			blocks = append(blocks, b)
			ecs = append(ecs, rsRemainder(b, t.ecPerBlock))
		}
	}
	var final []byte
	for i := 0; ; i++ {
		any := false
		for _, b := range blocks {
			if i < len(b) {
				final = append(final, b[i])
				any = true
			}
		}
		if !any {
			break
		}
	}
	for i := 0; i < t.ecPerBlock; i++ {
		for _, e := range ecs {
			final = append(final, e[i])
		}
	}
	q := newQR(ver)
	q.place(final)
	best, bestScore := 0, -1
	for mask := 0; mask < 8; mask++ {
		q.applyMask(mask)
		q.drawFormat(mask)
		if s := q.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = mask, s
		}
		q.applyMask(mask) // undo
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q, nil
}

func newQR(ver int) *Code {
	size := ver*4 + 17
	q := &Code{Size: size}
	q.modules = make([][]bool, size)
	q.fn = make([][]bool, size)
	for i := range q.modules {
		q.modules[i] = make([]bool, size)
		q.fn[i] = make([]bool, size)
	}
	set := func(x, y int, dark bool) { q.modules[y][x], q.fn[y][x] = dark, true }
	for i := 0; i < size; i++ { // timing patterns
		set(6, i, i%2 == 0)
		set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {size - 4, 3}, {3, size - 4}} { // finders and their separators
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x >= 0 && x < size && y >= 0 && y < size {
					d := max(abs(dx), abs(dy))
					set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	al := qrAlign[ver]
	for i, ay := range al {
		for j, ax := range al {
			if i == 0 && j == 0 || i == 0 && j == len(al)-1 || i == len(al)-1 && j == 0 {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					set(ax+dx, ay+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserves the format areas (redrawn once the mask is chosen)
	if ver >= 7 {
		rem := ver
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		v := ver<<12 | rem
		for i := 0; i < 18; i++ {
			bit := v>>i&1 == 1
			a, b := size-11+i%3, i/3
			set(a, b, bit)
			set(b, a, bit)
		}
	}
	return q
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// drawFormat writes the error correction level (M) and mask, twice.
func (q *Code) drawFormat(mask int) {
	data := 0<<3 | mask // M = 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	set := func(x, y int, dark bool) { q.modules[y][x], q.fn[y][x] = dark, true }
	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		set(q.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, q.Size-15+i, bit(i))
	}
	set(8, q.Size-8, true) // always dark
}

// place lays the codewords out in the zigzag order.
func (q *Code) place(data []byte) {
	i := 0
	for right := q.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.Size - 1 - vert
				}
				if !q.fn[y][x] && i < len(data)*8 {
					q.modules[y][x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (q *Code) applyMask(mask int) {
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if q.fn[y][x] {
				continue
			}
			var inv bool
			switch mask {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv {
				q.modules[y][x] = !q.modules[y][x]
			}
		}
	}
}

// penalty scores how hard the code is to read (lower is better).
func (q *Code) penalty() int {
	n := q.Size
	at := func(x, y int, col bool) bool {
		if col {
			return q.modules[x][y]
		}
		return q.modules[y][x]
	}
	score := 0
	for _, col := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 1
			for x := 1; x <= n; x++ {
				if x < n && at(x, y, col) == at(x-1, y, col) {
					run++
					continue
				}
				if run >= 5 {
					score += 3 + run - 5
				}
				run = 1
			}
			// finder-like patterns: 1011101 with four light modules on either side
			for x := 0; x+7 <= n; x++ {
				p := [7]bool{true, false, true, true, true, false, true}
				ok := true
				for k := 0; k < 7; k++ {
					if at(x+k, y, col) != p[k] {
						ok = false
						break
					}
				}
				if !ok {
					continue
				}
				light := func(a, b int) bool {
					for k := a; k < b; k++ {
						if k >= 0 && k < n && at(k, y, col) {
							return false
						}
					}
					return true
				}
				if light(x-4, x) || light(x+7, x+11) {
					score += 40
				}
			}
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.modules[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				c := q.modules[y][x]
				if q.modules[y][x+1] == c && q.modules[y+1][x] == c && q.modules[y+1][x+1] == c {
					score += 3
				}
			}
		}
	}
	pct := dark * 100 / (n * n)
	score += abs(pct-50) / 5 * 10
	return score
}

// rsRemainder computes Reed-Solomon error correction codewords over GF(256).
func rsRemainder(data []byte, n int) []byte {
	gen := []byte{1}
	root := byte(1)
	for i := 0; i < n; i++ { // (x - α^0)(x - α^1)…
		next := make([]byte, len(gen)+1)
		for j, c := range gen {
			next[j] ^= c
			next[j+1] ^= gfMul(c, root)
		}
		gen = next
		root = gfMul(root, 2)
	}
	rem := make([]byte, n)
	for _, b := range data {
		f := b ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for j := 0; j < n; j++ {
			rem[j] ^= gfMul(gen[j+1], f)
		}
	}
	return rem
}

func gfMul(a, b byte) byte {
	var p byte
	for b > 0 {
		if b&1 == 1 {
			p ^= a
		}
		hi := a & 0x80
		a <<= 1
		if hi != 0 {
			a ^= 0x1D
		}
		b >>= 1
	}
	return p
}

// SVG draws the code with a quiet zone, scaled to fill its box.
func (q *Code) SVG() string {
	const quiet = 4
	n := q.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if q.modules[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
