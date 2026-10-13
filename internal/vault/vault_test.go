package vault

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func roundtrip(t *testing.T, size int) {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	key := make([]byte, KeySize)
	rand.Read(key)
	ct, err := io.ReadAll(EncryptReader(bytes.NewReader(data), key))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := io.ReadAll(DecryptReader(bytes.NewReader(ct), key))
	if err != nil || !bytes.Equal(pt, data) {
		t.Fatalf("size %d: roundtrip failed: %v", size, err)
	}
}

func TestRoundtripSizes(t *testing.T) {
	for _, n := range []int{0, 1, SegmentSize - 1, SegmentSize, SegmentSize + 1, 2 * SegmentSize, 2*SegmentSize + 7} {
		roundtrip(t, n)
	}
}

func encrypt(t *testing.T, size int) ([]byte, []byte) {
	key := make([]byte, KeySize)
	rand.Read(key)
	data := make([]byte, size)
	rand.Read(data)
	ct, _ := io.ReadAll(EncryptReader(bytes.NewReader(data), key))
	return ct, key
}

func TestTamperTruncateReorder(t *testing.T) {
	ct, key := encrypt(t, 3*SegmentSize+10)
	dec := func(b []byte) error { _, err := io.ReadAll(DecryptReader(bytes.NewReader(b), key)); return err }

	flipped := append([]byte(nil), ct...)
	flipped[100] ^= 1
	if dec(flipped) != ErrCorrupt {
		t.Fatal("bit flip not detected")
	}
	seg := SegmentSize + tagSize
	if dec(ct[:seg*2]) != ErrCorrupt { // dropped tail segments
		t.Fatal("truncation at segment boundary not detected")
	}
	if dec(ct[:len(ct)-1]) != ErrCorrupt {
		t.Fatal("short tail not detected")
	}
	swapped := append(append(append([]byte(nil), ct[seg:2*seg]...), ct[:seg]...), ct[2*seg:]...)
	if dec(swapped) != ErrCorrupt {
		t.Fatal("reorder not detected")
	}
	if dec(append(append([]byte(nil), ct...), 0)) != ErrCorrupt {
		t.Fatal("trailing garbage not detected")
	}
}

func TestPassphraseAndMeta(t *testing.T) {
	salt := NewSalt()
	v := Unlock("correct horse", salt)
	check := v.MakeCheck()
	if err := Unlock("correct horse", salt).VerifyCheck(check); err != nil {
		t.Fatal(err)
	}
	if Unlock("wrong", salt).VerifyCheck(check) != ErrWrongPassphrase {
		t.Fatal("wrong passphrase accepted")
	}
	m := NewFileMeta("секрет.txt")
	m.Size = 42
	got, err := v.OpenMeta(v.SealMeta(m))
	if err != nil || got.Name != m.Name || got.Size != 42 || !bytes.Equal(got.Key, m.Key) {
		t.Fatalf("meta roundtrip: %v %+v", err, got)
	}
	if _, err := Unlock("wrong", salt).OpenMeta(v.SealMeta(m)); err == nil {
		t.Fatal("meta opened with wrong key")
	}
}
