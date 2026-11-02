package ui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Reed-Solomon vector of the specification's worked example ("HELLO
// WORLD", version 1-M): 16 data codewords give these 10 error-correction
// codewords.
func TestQRReedSolomonKnownVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, rsDivisor(10)); !bytes.Equal(got, want) {
		t.Fatalf("ecc %v, want %v", got, want)
	}
}

// Format information of level M with mask 0 is the specification's
// 101010000010010 (bit 14 first).
func TestQRFormatBitsAndStructure(t *testing.T) {
	q, err := qrEncode([]byte("otpauth://totp/CryoBackup:admin?secret=ABCDEFGHIJKLMNOPQRSTUVWXYZ234567&issuer=CryoBackup&algorithm=SHA1&digits=6&period=30"))
	if err != nil {
		t.Fatal(err)
	}
	if q.size < 21 || (q.size-17)%4 != 0 {
		t.Fatalf("size %d", q.size)
	}
	// Finder pattern corners are dark, separators light, the dark module is set.
	for _, c := range [][2]int{{0, 0}, {q.size - 1, 0}, {0, q.size - 1}} {
		if !q.modules[c[1]][c[0]] {
			t.Errorf("finder corner %v is light", c)
		}
	}
	if q.modules[7][7] || !q.modules[q.size-8][8] {
		t.Error("separator or dark module wrong")
	}
	// Timing pattern alternates.
	for i := 8; i < q.size-8; i++ {
		if q.modules[6][i] != (i%2 == 0) {
			t.Fatalf("timing at %d", i)
		}
	}
	// Recompute the format word from the first copy and check it decodes to M + a mask.
	bits := 0
	for i := 0; i <= 5; i++ {
		if q.modules[i][8] {
			bits |= 1 << i
		}
	}
	for i, xy := range [][2]int{{8, 7}, {8, 8}, {7, 8}} {
		if q.modules[xy[1]][xy[0]] {
			bits |= 1 << (6 + i)
		}
	}
	for i := 9; i < 15; i++ {
		if q.modules[8][14-i] {
			bits |= 1 << i
		}
	}
	data := (bits ^ 0x5412) >> 10
	if data>>3 != qrFormatM {
		t.Fatalf("format level bits %b", data>>3)
	}
	q0 := qrFormatM<<3 | 0
	rem := q0
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	if got := fmt.Sprintf("%015b", (q0<<10|rem)^0x5412); got != "101010000010010" {
		t.Fatalf("format M/0 = %s", got)
	}
}

func TestQRTooLongAndSVG(t *testing.T) {
	if _, err := qrEncode(bytes.Repeat([]byte("a"), 2332)); err == nil {
		t.Fatal("2332 bytes must not fit a version-40 symbol at level M")
	}
	if q, err := qrEncode(bytes.Repeat([]byte("a"), 2331)); err != nil || q.size != 177 {
		t.Fatalf("2331 bytes must fit version 40: %v", err)
	}
	// The data capacities of level M, byte mode, from the specification (table 7).
	for ver, want := range map[int]int{1: 14, 10: 213, 11: 251, 20: 666, 27: 1125, 32: 1538, 40: 2331} {
		cc := 8
		if ver >= 10 {
			cc = 16
		}
		if got := (qrDataCodewords(ver)*8 - 4 - cc) / 8; got != want {
			t.Errorf("version %d-M holds %d bytes, want %d", ver, got, want)
		}
	}
	svg, err := qrSVG("otpauth://totp/x", "QR code")
	if err != nil || !strings.HasPrefix(string(svg), `<svg class="qr"`) || !strings.Contains(string(svg), `class="qr-ink"`) {
		t.Fatalf("svg: %v %.80s", err, svg)
	}
}

// TestQRWritePGM writes symbols as PGM images for an independent decoder
// (CRYO_QR_PGM_DIR set; skipped otherwise). The M2-W1A check decoded them with
// OpenCV's QRCodeDetector.
func TestQRWritePGM(t *testing.T) {
	dir := os.Getenv("CRYO_QR_PGM_DIR")
	if dir == "" {
		t.Skip("CRYO_QR_PGM_DIR not set")
	}
	for i, text := range []string{
		"HELLO WORLD",
		"otpauth://totp/CryoBackup:admin?algorithm=SHA1&digits=6&issuer=CryoBackup&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
		"otpauth://totp/CryoBackup:a.very.long.username%40example-msp.internal?algorithm=SHA1&digits=6&issuer=CryoBackup&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
		strings.Repeat("x", 200),
		"otpauth://totp/CryoBackup:" + strings.Repeat("u", 100) + "?algorithm=SHA1&digits=6&issuer=CryoBackup&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
		strings.Repeat("y", 400),  // version 15
		strings.Repeat("z", 1000), // version 26
		strings.Repeat("w", 1300), // version 30
		strings.Repeat("v", 1500), // version 32 (the irregular alignment step)
	} {
		q, err := qrEncode([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		const scale, quiet = 8, 4
		dim := (q.size + 2*quiet) * scale
		var b bytes.Buffer
		fmt.Fprintf(&b, "P5\n%d %d\n255\n", dim, dim)
		for py := 0; py < dim; py++ {
			for px := 0; px < dim; px++ {
				x, y := px/scale-quiet, py/scale-quiet
				v := byte(255)
				if x >= 0 && y >= 0 && x < q.size && y < q.size && q.modules[y][x] {
					v = 0
				}
				b.WriteByte(v)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("qr%d.pgm", i)), b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("qr%d.txt", i)), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
