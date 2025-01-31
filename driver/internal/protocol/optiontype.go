// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"fmt"

	"github.com/SAP/go-hdb/driver/internal/protocol/encoding"
)

type optType interface {
	fmt.Stringer
	typeCode() TypeCode
	size(v interface{}) int
	encode(e *encoding.Encoder, v interface{})
	decode(d *encoding.Decoder) interface{}
}

var (
	optBooleanType = _optBooleanType{}
	optTinyintType = _optTinyintType{}
	optIntegerType = _optIntegerType{}
	optBigintType  = _optBigintType{}
	optDoubleType  = _optDoubleType{}
	optStringType  = _optStringType{}
	optBstringType = _optBstringType{}
)

type (
	_optBooleanType struct{}
	_optTinyintType struct{}
	_optIntegerType struct{}
	_optBigintType  struct{}
	_optDoubleType  struct{}
	_optStringType  struct{}
	_optBstringType struct{}
)

var (
	_ optType = (*_optBooleanType)(nil)
	_ optType = (*_optTinyintType)(nil)
	_ optType = (*_optIntegerType)(nil)
	_ optType = (*_optBigintType)(nil)
	_ optType = (*_optDoubleType)(nil)
	_ optType = (*_optStringType)(nil)
	_ optType = (*_optBstringType)(nil)
)

func (_optBooleanType) String() string { return "booleanType" }
func (_optTinyintType) String() string { return "tinyintType" }
func (_optIntegerType) String() string { return "integerType" }
func (_optBigintType) String() string  { return "bigintType" }
func (_optDoubleType) String() string  { return "doubleType" }
func (_optStringType) String() string  { return "dateType" }
func (_optBstringType) String() string { return "timeType" }

func (_optBooleanType) typeCode() TypeCode { return tcBoolean }
func (_optTinyintType) typeCode() TypeCode { return tcTinyint }
func (_optIntegerType) typeCode() TypeCode { return tcInteger }
func (_optBigintType) typeCode() TypeCode  { return tcBigint }
func (_optDoubleType) typeCode() TypeCode  { return tcDouble }
func (_optStringType) typeCode() TypeCode  { return tcString }
func (_optBstringType) typeCode() TypeCode { return tcBstring }

func (_optBooleanType) size(interface{}) int   { return encoding.BooleanFieldSize }
func (_optTinyintType) size(interface{}) int   { return encoding.TinyintFieldSize }
func (_optIntegerType) size(interface{}) int   { return encoding.IntegerFieldSize }
func (_optBigintType) size(interface{}) int    { return encoding.BigintFieldSize }
func (_optDoubleType) size(interface{}) int    { return encoding.DoubleFieldSize }
func (_optStringType) size(v interface{}) int  { return 2 + len(v.(string)) } //length int16 + string length
func (_optBstringType) size(v interface{}) int { return 2 + len(v.([]byte)) } //length int16 + bytes length

func (_optBooleanType) encode(e *encoding.Encoder, v interface{}) { e.Bool(v.(bool)) }
func (_optTinyintType) encode(e *encoding.Encoder, v interface{}) { e.Int8(v.(int8)) }
func (_optIntegerType) encode(e *encoding.Encoder, v interface{}) { e.Int32(v.(int32)) }
func (_optBigintType) encode(e *encoding.Encoder, v interface{})  { e.Int64(v.(int64)) }
func (_optDoubleType) encode(e *encoding.Encoder, v interface{})  { e.Float64(v.(float64)) }
func (_optStringType) encode(e *encoding.Encoder, v interface{}) {
	s := v.(string)
	e.Int16(int16(len(s)))
	e.Bytes([]byte(s))
}
func (_optBstringType) encode(e *encoding.Encoder, v interface{}) {
	b := v.([]byte)
	e.Int16(int16(len(b)))
	e.Bytes(b)
}

func (_optBooleanType) decode(d *encoding.Decoder) interface{} { return d.Bool() }
func (_optTinyintType) decode(d *encoding.Decoder) interface{} { return d.Int8() }
func (_optIntegerType) decode(d *encoding.Decoder) interface{} { return d.Int32() }
func (_optBigintType) decode(d *encoding.Decoder) interface{}  { return d.Int64() }
func (_optDoubleType) decode(d *encoding.Decoder) interface{}  { return d.Float64() }
func (_optStringType) decode(d *encoding.Decoder) interface{} {
	l := d.Int16()
	b := make([]byte, l)
	d.Bytes(b)
	return string(b)
}
func (_optBstringType) decode(d *encoding.Decoder) interface{} {
	l := d.Int16()
	b := make([]byte, l)
	d.Bytes(b)
	return b
}

func getOptType(v interface{}) optType {
	switch v := v.(type) {
	case bool:
		return optBooleanType
	case int8:
		return optTinyintType
	case int32:
		return optIntegerType
	case int64:
		return optBigintType
	case float64:
		return optDoubleType
	case string:
		return optStringType
	case []byte:
		return optBstringType
	default:
		panic(fmt.Sprintf("type %T not implemented", v)) // should never happen
	}
}
