// Package uuid parses and formats RFC 4122 ids. It exists so record lookups can
// reject a malformed id before it reaches Postgres, without a new dependency.
package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
)

type UUID [16]byte

var Nil UUID

var ErrInvalid = errors.New("invalid uuid")

// Parse accepts the canonical 8-4-4-4-12 form, in either case.
func Parse(s string) (UUID, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return Nil, ErrInvalid
	}
	var u UUID
	src := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	if _, err := hex.Decode(u[:], []byte(src)); err != nil {
		return Nil, ErrInvalid
	}
	return u, nil
}

func (u UUID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u[10:16])
	return string(buf[:])
}

func (u UUID) IsZero() bool { return u == Nil }

// NewString returns a random version 4 UUID. It is used for ids the database
// does not generate, such as an outbox event written from application code.
func NewString() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on; if it ever
		// did, a predictable id would be worse than stopping.
		panic("uuid: " + err.Error())
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return UUID(buffer).String()
}
