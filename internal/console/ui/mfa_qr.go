package ui

import (
	"errors"
	"fmt"
	"html/template"
	"strings"
)

// A minimal QR code encoder (ISO/IEC 18004) for the TOTP enrollment page
// (components §9.8: "QR is an inline SVG generated server-side"). It encodes
// one byte-mode segment at error-correction level M in any version, 1 to 40
// (up to 2331 bytes; an otpauth:// URI is about 130, about 230 for the longest
// username the account form accepts), picks the mask with the lowest
// penalty score, and renders an inline SVG. It exists so the console needs no
// QR dependency and no client-side library; the structure follows the public
// specification as laid out by Project Nayuki's reference generator.
//
// The QR carries the TOTP secret: it is rendered into a no-store page once and
// never cached, logged or written anywhere.

var errQRTooLong = errors.New("qr: data too long for a version-40 symbol at level M")

// Level M parameters per version (index 0 unused).
var (
	qrECCPerBlockM = [41]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26,
		26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28}
	qrBlocksM = [41]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16,
		17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49}
)

const qrFormatM = 0 // the two-bit format code of level M

type qrCode struct {
	size    int
	modules [][]bool
	fn      [][]bool // function modules (finder, timing, alignment, format, version)
}

func qrRawModules(ver int) int {
	n := (16*ver+128)*ver + 64
	if ver >= 2 {
		na := ver/7 + 2
		n -= (25*na-10)*na - 55
		if ver >= 7 {
			n -= 36
		}
	}
	return n
}

func qrDataCodewords(ver int) int {
	return qrRawModules(ver)/8 - qrECCPerBlockM[ver]*qrBlocksM[ver]
}

func qrAlignPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	na := ver/7 + 2
	step := (ver*4 + na*2 + 1) / (na*2 - 2) * 2
	if ver == 32 {
		step = 26 // the one version where the general formula is off (specification table E.1)
	}
	out := make([]int, na)
	out[0] = 6
	for i, pos := na-1, ver*4+10; i >= 1; i, pos = i-1, pos-step {
		out[i] = pos
	}
	return out
}

// ---- Reed-Solomon over GF(2^8), polynomial 0x11D ----

func gfMul(x, y byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int((y>>uint(i))&1) * int(x)
	}
	return byte(z)
}

func rsDivisor(degree int) []byte {
	res := make([]byte, degree)
	res[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range res {
			res[j] = gfMul(res[j], root)
			if j+1 < len(res) {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return res
}

func rsRemainder(data, div []byte) []byte {
	res := make([]byte, len(div))
	for _, b := range data {
		factor := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i := range res {
			res[i] ^= gfMul(div[i], factor)
		}
	}
	return res
}

// ---- encoding ----

// qrEncode builds the symbol for data in the smallest version that fits.
func qrEncode(data []byte) (*qrCode, error) {
	ver := 0
	for v := 1; v <= 40; v++ {
		ccBits := 8
		if v >= 10 {
			ccBits = 16
		}
		if 4+ccBits+8*len(data) <= qrDataCodewords(v)*8 {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errQRTooLong
	}
	ccBits := 8
	if ver >= 10 {
		ccBits = 16
	}
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>uint(i))&1 == 1)
		}
	}
	put(0x4, 4) // byte mode
	put(len(data), ccBits)
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := qrDataCodewords(ver) * 8
	put(0, min(4, capBits-len(bits)))
	put(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	cw := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			cw[i>>3] |= 1 << uint(7-i&7)
		}
	}

	q := &qrCode{size: ver*4 + 17}
	q.modules, q.fn = make([][]bool, q.size), make([][]bool, q.size)
	for i := range q.modules {
		q.modules[i], q.fn[i] = make([]bool, q.size), make([]bool, q.size)
	}
	q.drawFunctionPatterns(ver)
	q.drawCodewords(qrInterleave(ver, cw))

	best, bestPenalty := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(m)
		if p := q.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
		q.applyMask(m) // XOR undoes it
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q, nil
}

func qrInterleave(ver int, data []byte) []byte {
	nb, ecc := qrBlocksM[ver], qrECCPerBlockM[ver]
	raw := qrRawModules(ver) / 8
	short := nb - raw%nb
	shortLen := raw / nb
	div := rsDivisor(ecc)
	blocks := make([][]byte, nb)
	for i, k := 0, 0; i < nb; i++ {
		n := shortLen - ecc
		if i >= short {
			n++
		}
		dat := data[k : k+n]
		k += n
		block := make([]byte, shortLen+1)
		copy(block, dat)
		copy(block[len(block)-ecc:], rsRemainder(dat, div))
		blocks[i] = block
	}
	out := make([]byte, 0, raw)
	for i := 0; i < shortLen+1; i++ {
		for j := range blocks {
			if i != shortLen-ecc || j >= short {
				out = append(out, blocks[j][i])
			}
		}
	}
	return out
}

func (q *qrCode) set(x, y int, dark bool) {
	q.modules[y][x] = dark
	q.fn[y][x] = true
}

func (q *qrCode) drawFunctionPatterns(ver int) {
	for i := 0; i < q.size; i++ {
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {q.size - 4, 3}, {3, q.size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x >= 0 && x < q.size && y >= 0 && y < q.size {
					d := max(abs(dx), abs(dy))
					q.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	al := qrAlignPositions(ver)
	for i := range al {
		for j := range al {
			if (i == 0 && j == 0) || (i == 0 && j == len(al)-1) || (i == len(al)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.set(al[i]+dx, al[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserves the format area; the real bits come with the mask
	if ver >= 7 {
		rem := ver
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := ver<<12 | rem
		for i := 0; i < 18; i++ {
			dark := (bits>>uint(i))&1 == 1
			a, b := q.size-11+i%3, i/3
			q.set(a, b, dark)
			q.set(b, a, dark)
		}
	}
}

func (q *qrCode) drawFormat(mask int) {
	data := qrFormatM<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>uint(i))&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(q.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, q.size-15+i, bit(i))
	}
	q.set(8, q.size-8, true) // the dark module
}

func (q *qrCode) drawCodewords(data []byte) {
	i := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.size - 1 - vert
				}
				if !q.fn[y][x] && i < len(data)*8 {
					q.modules[y][x] = (data[i>>3]>>uint(7-i&7))&1 == 1
					i++
				}
			}
		}
	}
}

func qrMaskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func (q *qrCode) applyMask(mask int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if !q.fn[y][x] && qrMaskBit(mask, x, y) {
				q.modules[y][x] = !q.modules[y][x]
			}
		}
	}
}

// penalty is the specification's mask score (rules N1 to N4).
func (q *qrCode) penalty() int {
	n := q.size
	at := func(x, y int, transpose bool) bool {
		if transpose {
			return q.modules[x][y]
		}
		return q.modules[y][x]
	}
	p := 0
	for _, tr := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 1
			for x := 1; x <= n; x++ {
				if x < n && at(x, y, tr) == at(x-1, y, tr) {
					run++
					continue
				}
				if run >= 5 {
					p += 3 + run - 5
				}
				run = 1
			}
			// N3: 1:1:3:1:1 finder-like pattern with four light modules on one side.
			for x := 0; x+11 <= n; x++ {
				var s strings.Builder
				for k := 0; k < 11; k++ {
					if at(x+k, y, tr) {
						s.WriteByte('1')
					} else {
						s.WriteByte('0')
					}
				}
				if s.String() == "10111010000" || s.String() == "00001011101" {
					p += 40
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
					p += 3
				}
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	return p + k*10
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// qrSVG renders text as an inline SVG QR code with a four-module quiet zone.
// Colours come from the .qr rules in app.css (tokens only), dark modules on a
// light ground in both themes so every scanner reads it.
func qrSVG(text, label string) (template.HTML, error) {
	q, err := qrEncode([]byte(text))
	if err != nil {
		return "", err
	}
	const quiet = 4
	dim := q.size + 2*quiet
	var path strings.Builder
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.modules[y][x] {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return template.HTML(fmt.Sprintf(`<svg class="qr" role="img" aria-label="%s" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect class="qr-ground" width="%d" height="%d"/><path class="qr-ink" d="%s"/></svg>`, //nolint:gosec // every value is ours: numbers, a fixed label, the path
		template.HTMLEscapeString(label), dim, dim, dim, dim, path.String())), nil
}
