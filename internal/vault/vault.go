// Package vault implements the client-side cryptography. Everything that
// leaves the user's machine is ciphertext; the coordinator and donor nodes
// never see plaintext, file names or keys.
//
// Key hierarchy:
//
//	passphrase --Argon2id(salt)--> master key (never leaves memory)
//	master key --AES-256-GCM--> encrypted metadata blob {name, size, file key}
//	file key (random, per file) --STREAM(AES-256-GCM, 1 MiB segments)--> content
//
// The STREAM construction authenticates every segment, its position and the
// end of the stream, so reordering, dropping or truncating segments is detected.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"golang.org/x/crypto/argon2"
)

const (
	SegmentSize = 1 << 20
	KeySize     = 32
	SaltSize    = 16
	tagSize     = 16
	metaVersion = 1

	checkPlain = "p2pdrive-key-check-v1"
)

var (
	ErrWrongPassphrase = errors.New("wrong passphrase")
	ErrCorrupt         = errors.New("data is corrupted or has been tampered with")
)

// Meta is the private description of a stored file.
type Meta struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Key  []byte `json:"key"`
}

// Vault holds the unlocked master key.
type Vault struct{ master []byte }

func NewSalt() []byte {
	s := make([]byte, SaltSize)
	mustRand(s)
	return s
}

// Unlock derives the master key from the passphrase.
func Unlock(passphrase string, salt []byte) *Vault {
	return &Vault{master: argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, KeySize)}
}

// MakeCheck returns a blob that lets a later Unlock verify the passphrase.
func (v *Vault) MakeCheck() []byte { return v.seal([]byte(checkPlain)) }

func (v *Vault) VerifyCheck(check []byte) error {
	pt, err := v.open(check)
	if err != nil || subtle.ConstantTimeCompare(pt, []byte(checkPlain)) != 1 {
		return ErrWrongPassphrase
	}
	return nil
}

// NewFileMeta creates metadata with a fresh random file key.
func NewFileMeta(name string) *Meta {
	k := make([]byte, KeySize)
	mustRand(k)
	return &Meta{Name: name, Key: k}
}

func (v *Vault) SealMeta(m *Meta) []byte {
	b, _ := json.Marshal(m)
	return append([]byte{metaVersion}, v.seal(b)...)
}

func (v *Vault) OpenMeta(blob []byte) (*Meta, error) {
	if len(blob) < 1 || blob[0] != metaVersion {
		return nil, ErrCorrupt
	}
	pt, err := v.open(blob[1:])
	if err != nil {
		return nil, ErrCorrupt
	}
	var m Meta
	if err := json.Unmarshal(pt, &m); err != nil || len(m.Key) != KeySize {
		return nil, ErrCorrupt
	}
	return &m, nil
}

// seal / open: AES-256-GCM with a random 96-bit nonce prepended.
func (v *Vault) seal(pt []byte) []byte {
	g := newGCM(v.master)
	nonce := make([]byte, g.NonceSize())
	mustRand(nonce)
	return g.Seal(nonce, nonce, pt, nil)
}

func (v *Vault) open(blob []byte) ([]byte, error) {
	g := newGCM(v.master)
	if len(blob) < g.NonceSize()+tagSize {
		return nil, ErrCorrupt
	}
	return g.Open(nil, blob[:g.NonceSize()], blob[g.NonceSize():], nil)
}

func newGCM(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

func mustRand(b []byte) {
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
}

// segNonce builds the STREAM nonce: 11-byte big-endian counter || last-flag.
func segNonce(ctr uint64, last bool) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[3:11], ctr)
	if last {
		n[11] = 1
	}
	return n
}

// EncryptReader returns a reader producing the ciphertext of src.
func EncryptReader(src io.Reader, fileKey []byte) io.Reader {
	return &encReader{src: src, aead: newGCM(fileKey), buf: make([]byte, 0, SegmentSize+1)}
}

type encReader struct {
	src  io.Reader
	aead cipher.AEAD
	buf  []byte // lookahead plaintext, up to SegmentSize+1 bytes
	out  []byte
	ctr  uint64
	done bool
	err  error
}

func (r *encReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.fill()
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *encReader) fill() {
	have := len(r.buf)
	r.buf = r.buf[:SegmentSize+1]
	n, err := io.ReadFull(r.src, r.buf[have:])
	total := have + n
	switch {
	case err == nil: // a full segment plus one lookahead byte: not the last
		r.out = r.aead.Seal(nil, segNonce(r.ctr, false), r.buf[:SegmentSize], nil)
		r.buf[0] = r.buf[SegmentSize]
		r.buf = r.buf[:1]
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		r.out = r.aead.Seal(nil, segNonce(r.ctr, true), r.buf[:total], nil)
		r.done = true
	default:
		r.err = err
		return
	}
	r.ctr++
}

// DecryptReader returns a reader producing the plaintext. Errors surface as
// ErrCorrupt once a bad or missing segment is reached.
func DecryptReader(src io.Reader, fileKey []byte) io.Reader {
	return &decReader{src: src, aead: newGCM(fileKey), buf: make([]byte, 0, SegmentSize+tagSize+1)}
}

type decReader struct {
	src  io.Reader
	aead cipher.AEAD
	buf  []byte
	out  []byte
	ctr  uint64
	done bool
	err  error
}

func (r *decReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.fill()
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *decReader) fill() {
	const seg = SegmentSize + tagSize
	have := len(r.buf)
	r.buf = r.buf[:seg+1]
	n, err := io.ReadFull(r.src, r.buf[have:])
	total := have + n
	last := false
	var ct []byte
	switch {
	case err == nil:
		ct = r.buf[:seg]
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		last, ct = true, r.buf[:total]
	default:
		r.err = err
		return
	}
	pt, oerr := r.aead.Open(nil, segNonce(r.ctr, last), ct, nil)
	if oerr != nil {
		r.err = ErrCorrupt
		return
	}
	r.out = pt
	if last {
		r.done = true
	} else {
		r.buf[0] = r.buf[seg]
		r.buf = r.buf[:1]
	}
	r.ctr++
}
