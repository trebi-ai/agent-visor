package holder

import (
	"bytes"
	"testing"
)

func TestRingKeepsLast(t *testing.T) {
	t.Parallel()
	r := NewRing(8)
	r.Write([]byte("abcdef"))
	r.Write([]byte("ghij"))
	if got := string(r.Bytes()); got != "cdefghij" {
		t.Fatalf("bytes %q", got)
	}
	var all []byte
	all = append(all, "abcdefghij"...)
	for i := range 10 {
		chunk := []byte{byte('A' + i*3), byte('A' + i*3 + 1), byte('A' + i*3 + 2)}
		r.Write(chunk)
		all = append(all, chunk...)
		if want := all[len(all)-8:]; !bytes.Equal(r.Bytes(), want) {
			t.Fatalf("bytes %q, want %q", r.Bytes(), want)
		}
		if r.Len() != 8 {
			t.Fatalf("len %d", r.Len())
		}
		if len(r.buf) > 2*8+3 {
			t.Fatalf("buffer %d", len(r.buf))
		}
	}
}

func TestRingZeroValue(t *testing.T) {
	t.Parallel()
	var r Ring
	r.Write(bytes.Repeat([]byte("x"), DefaultRing+100))
	if r.Len() != DefaultRing || len(r.Bytes()) != DefaultRing {
		t.Fatalf("len %d", r.Len())
	}
}

func TestRingReset(t *testing.T) {
	t.Parallel()
	r := NewRing(8)
	r.Reset([]byte("0123456789abcdefghij"))
	if got := string(r.Bytes()); got != "cdefghij" {
		t.Fatalf("bytes %q", got)
	}
	r.Write([]byte("k"))
	if got := string(r.Bytes()); got != "defghijk" {
		t.Fatalf("bytes %q", got)
	}
}

func BenchmarkRingWrite(b *testing.B) {
	r := NewRing(DefaultRing)
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	for range 40 {
		r.Write(chunk)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r.Write(chunk)
	}
}
