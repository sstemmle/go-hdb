// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

// Package auth provides authentication methods.
package auth

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/SAP/go-hdb/driver/internal/protocol/encoding"
	"github.com/SAP/go-hdb/driver/unicode/cesu8"
)

// authentication method types supported by the driver:
// - basic authentication (username, password based) (whether SCRAMSHA256 or SCRAMPBKDF2SHA256) and
// - X509 (client certificate) authentication and
// - JWT (token) authentication
const (
	MtSCRAMSHA256       = "SCRAMSHA256"       // password
	MtSCRAMPBKDF2SHA256 = "SCRAMPBKDF2SHA256" // password pbkdf2
	MtX509              = "X509"              // client certificate
	MtJWT               = "JWT"               // json web token
	MtSessionCookie     = "SessionCookie"     // session cookie
)

// authentication method orders.
const (
	MoSessionCookie byte = iota
	MoX509
	MoJWT
	MoSCRAMPBKDF2SHA256
	MoSCRAMSHA256
)

// A Method defines the interface for an authentication method.
type Method interface {
	fmt.Stringer
	Typ() string
	Order() byte
	PrepareInitReq(prms *Prms)
	InitRepDecode(d *Decoder) error
	PrepareFinalReq(prms *Prms) error
	FinalRepDecode(d *Decoder) error
}

var (
	_ Method = (*SCRAMSHA256)(nil)
	_ Method = (*SCRAMPBKDF2SHA256)(nil)
	_ Method = (*JWT)(nil)
	_ Method = (*X509)(nil)
	_ Method = (*SessionCookie)(nil)
)

// subPrmsSize is the type used to encode and decode the size of sub parameters.
// The hana protocoll supports whether:
//   - a size <= 245 encoded in one byte or
//   - an unsigned 2 byte integer size encoded in three bytes
//     . first byte equals 255
//     . second and third byte is an big endian encoded uint16
type subPrmsSize int

const (
	maxSubPrmsSize1ByteLen    = 245
	subPrmsSize2ByteIndicator = 255
)

func (s subPrmsSize) fieldSize() int {
	if s > maxSubPrmsSize1ByteLen {
		return 3
	}
	return 1
}

func (s subPrmsSize) encode(e *encoding.Encoder) error {
	switch {
	case s <= maxSubPrmsSize1ByteLen:
		e.Byte(byte(s))
	case s <= math.MaxUint16:
		e.Byte(subPrmsSize2ByteIndicator)
		e.Uint16ByteOrder(uint16(s), binary.BigEndian) // big endian
	default:
		return fmt.Errorf("invalid subparameter size %d - maximum %d", s, 42)
	}
	return nil
}

func (s *subPrmsSize) decode(d *encoding.Decoder) {
	b := d.Byte()
	switch {
	case b <= maxSubPrmsSize1ByteLen:
		*s = subPrmsSize(b)
	case b == subPrmsSize2ByteIndicator:
		*s = subPrmsSize(d.Uint16ByteOrder(binary.BigEndian))
	default:
		panic(fmt.Sprintf("invalid sub parameter size indicator %d", b))
	}
}

// Decoder represents an authentication decoder.
type Decoder struct {
	d *encoding.Decoder
}

// NewDecoder returns a new decoder instance.
func NewDecoder(d *encoding.Decoder) *Decoder {
	return &Decoder{d: d}
}

// NumPrm ckecks the number of paramters and returns an error if not equal expected, nil otherwise.
func (d *Decoder) NumPrm(expected int) error {
	numPrm := int(d.d.Int16())
	if numPrm != expected {
		return fmt.Errorf("invalid number of parameters %d - expected %d", numPrm, expected)
	}
	return nil
}

func (d *Decoder) String() string               { _, s := d.d.LIString(); return s }
func (d *Decoder) cesu8String() (string, error) { _, s, err := d.d.CESU8LIString(); return s, err }
func (d *Decoder) bytes() []byte                { _, b := d.d.LIBytes(); return b }
func (d *Decoder) bigUint32() (uint32, error) {
	size := d.d.Byte()
	if size != encoding.IntegerFieldSize { // 4 bytes
		return 0, fmt.Errorf("invalid auth uint32 size %d - expected %d", size, encoding.IntegerFieldSize)
	}
	return d.d.Uint32ByteOrder(binary.BigEndian), nil // big endian coded (e.g. rounds param)
}
func (d *Decoder) subSize() int {
	var subSize subPrmsSize
	(&subSize).decode(d.d)
	return int(subSize)
}

// Prms represents authentication parameters.
type Prms struct {
	prms []interface{}
}

func (p *Prms) String() string { return fmt.Sprintf("%v", []interface{}(p.prms)) }

// AddCESU8String adds a CESU8 string parameter.
func (p *Prms) AddCESU8String(s string) { p.prms = append(p.prms, s) } // unicode string
func (p *Prms) addEmpty()               { p.prms = append(p.prms, []byte{}) }
func (p *Prms) addBytes(b []byte)       { p.prms = append(p.prms, b) }
func (p *Prms) addString(s string)      { p.prms = append(p.prms, []byte(s)) } // treat like bytes to distinguisch from unicode string
func (p *Prms) addPrms() *Prms {
	prms := &Prms{}
	p.prms = append(p.prms, prms)
	return prms
}

// Size returns the size in bytes of the parameters.
func (p *Prms) Size() int {
	size := encoding.SmallintFieldSize // no of parameters (2 bytes)
	for _, e := range p.prms {
		switch e := e.(type) {
		case []byte:
			size += encoding.VarBytesSize(len(e))
		case string:
			size += encoding.VarBytesSize(cesu8.StringSize(e))
		case *Prms:
			subSize := subPrmsSize(e.Size())
			size += (int(subSize) + subSize.fieldSize())
		default:
			panic(fmt.Sprintf("invalid parameter %[1]v %[1]t", e)) // should not happen
		}
	}
	return size
}

// Encode encodes the parameters.
func (p *Prms) Encode(enc *encoding.Encoder) error {
	numPrms := len(p.prms)
	if numPrms > math.MaxInt16 {
		return fmt.Errorf("invalid number of parameters %d - maximum %d", numPrms, math.MaxInt16)
	}
	enc.Int16(int16(numPrms))

	for _, e := range p.prms {
		switch e := e.(type) {
		case []byte:
			if err := enc.LIBytes(e); err != nil {
				return err
			}
		case string:
			if err := enc.CESU8LIString(e); err != nil {
				return err
			}
		case *Prms:
			subSize := subPrmsSize(e.Size())
			if err := subSize.encode(enc); err != nil {
				return err
			}
			if err := e.Encode(enc); err != nil {
				return err
			}
		default:
			panic(fmt.Sprintf("invalid parameter %[1]v %[1]t", e)) // should not happen
		}
	}
	return nil
}

func checkAuthMethodType(mt, expected string) error {
	if mt != expected {
		return fmt.Errorf("invalid method %s - expected %s", mt, expected)
	}
	return nil
}
