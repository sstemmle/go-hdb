// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"database/sql/driver"
	"fmt"
	"reflect"

	"github.com/SAP/go-hdb/driver/internal/protocol/encoding"
	"golang.org/x/text/transform"
)

type parameterOptions int8

const (
	poMandatory parameterOptions = 0x01
	poOptional  parameterOptions = 0x02
	poDefault   parameterOptions = 0x04
)

var parameterOptionsText = map[parameterOptions]string{
	poMandatory: "mandatory",
	poOptional:  "optional",
	poDefault:   "default",
}

func (k parameterOptions) String() string {
	t := make([]string, 0, len(parameterOptionsText))

	for option, text := range parameterOptionsText {
		if (k & option) != 0 {
			t = append(t, text)
		}
	}
	return fmt.Sprintf("%v", t)
}

// ParameterMode represents the parameter mode set.
type ParameterMode int8

// ParameterMode constants.
const (
	PmIn    ParameterMode = 0x01
	pmInout ParameterMode = 0x02
	PmOut   ParameterMode = 0x04
)

var parameterModeText = []string{
	PmIn:    "in",
	pmInout: "inout",
	PmOut:   "out",
}

func (k ParameterMode) String() string {
	t := make([]string, 0, len(parameterModeText))

	for mode, text := range parameterModeText {
		if (k & ParameterMode(mode)) != 0 {
			t = append(t, text)
		}
	}
	return fmt.Sprintf("%v", t)
}

func newParameterFields(size int) []*ParameterField {
	return make([]*ParameterField, size)
}

// ParameterField contains database field attributes for parameters.
type ParameterField struct {
	// field alignment
	FieldName        string
	ft               fieldType // avoid tc.fieldType() calls in Converter (e.g. bulk insert)
	Offset           uint32
	length           int16
	fraction         int16
	parameterOptions parameterOptions
	TC               TypeCode
	Mode             ParameterMode
}

func (f *ParameterField) String() string {
	return fmt.Sprintf("parameterOptions %s typeCode %s mode %s fraction %d length %d name %s",
		f.parameterOptions,
		f.TC,
		f.Mode,
		f.fraction,
		f.length,
		f.FieldName,
	)
}

// Convert returns the result of the fieldType conversion.
func (f *ParameterField) Convert(t transform.Transformer, v interface{}) (interface{}, error) {
	switch ft := f.ft.(type) {
	case fieldConverter:
		return ft.convert(v)
	case cesu8FieldConverter:
		return ft.convertCESU8(t, v)
	default:
		panic(fmt.Sprintf("field type %v does not implement converter", ft)) // should never happen
	}
}

// TypeName returns the type name of the field.
// see https://golang.org/pkg/database/sql/driver/#RowsColumnTypeDatabaseTypeName
func (f *ParameterField) TypeName() string { return f.TC.typeName() }

// ScanType returns the scan type of the field.
// see https://golang.org/pkg/database/sql/driver/#RowsColumnTypeScanType
func (f *ParameterField) ScanType() reflect.Type { return f.TC.dataType().ScanType() }

// TypeLength returns the type length of the field.
// see https://golang.org/pkg/database/sql/driver/#RowsColumnTypeLength
func (f *ParameterField) TypeLength() (int64, bool) {
	if f.TC.isVariableLength() {
		return int64(f.length), true
	}
	return 0, false
}

// TypePrecisionScale returns the type precision and scale (decimal types) of the field.
// see https://golang.org/pkg/database/sql/driver/#RowsColumnTypePrecisionScale
func (f *ParameterField) TypePrecisionScale() (int64, int64, bool) {
	if f.TC.isDecimalType() {
		return int64(f.length), int64(f.fraction), true
	}
	return 0, 0, false
}

// Nullable returns true if the field may be null, false otherwise.
// see https://golang.org/pkg/database/sql/driver/#RowsColumnTypeNullable
func (f *ParameterField) Nullable() bool {
	return f.parameterOptions == poOptional
}

// In returns true if the parameter field is an input field.
func (f *ParameterField) In() bool {
	return f.Mode == pmInout || f.Mode == PmIn
}

// Out returns true if the parameter field is an output field.
func (f *ParameterField) Out() bool {
	return f.Mode == pmInout || f.Mode == PmOut
}

// Name returns the parameter field name.
func (f *ParameterField) Name() string {
	return f.FieldName
}

func (f *ParameterField) decode(dec *encoding.Decoder) {
	f.parameterOptions = parameterOptions(dec.Int8())
	f.TC = TypeCode(dec.Int8())
	f.Mode = ParameterMode(dec.Int8())
	dec.Skip(1) //filler
	f.Offset = dec.Uint32()
	f.length = dec.Int16()
	f.fraction = dec.Int16()
	dec.Skip(4) //filler
	f.ft = f.TC.fieldType(int(f.length), int(f.fraction))
}

func (f *ParameterField) prmSize(v interface{}) int {
	if v == nil && f.TC.supportNullValue() {
		return 0
	}
	return f.ft.prmSize(v)
}

func (f *ParameterField) encodePrm(enc *encoding.Encoder, v interface{}) error {
	encTc := f.TC.encTc()
	if v == nil && f.TC.supportNullValue() {
		enc.Byte(byte(f.TC.nullValue())) // null value type code
		return nil
	}
	enc.Byte(byte(encTc)) // type code
	return f.ft.encodePrm(enc, v)
}

func (f *ParameterField) decodeRes(dec *encoding.Decoder) (interface{}, error) {
	return f.ft.decodeRes(dec)
}

/*
decode parameter
- currently not used
- type code is first byte (see encodePrm)
*/
func (f *ParameterField) decodePrm(dec *encoding.Decoder) (interface{}, error) {
	tc := TypeCode(dec.Byte())
	if tc&0x80 != 0 { // high bit set -> null value
		return nil, nil
	}
	return f.ft.decodePrm(dec)
}

// ParameterMetadata represents the metadata of a paramter.
type ParameterMetadata struct {
	ParameterFields []*ParameterField
}

func (m *ParameterMetadata) String() string {
	return fmt.Sprintf("parameter %v", m.ParameterFields)
}

func (m *ParameterMetadata) decode(dec *encoding.Decoder, ph *PartHeader) error {
	m.ParameterFields = newParameterFields(ph.numArg())

	names := fieldNames{}

	for i := 0; i < len(m.ParameterFields); i++ {
		f := new(ParameterField)
		f.decode(dec)
		m.ParameterFields[i] = f
		names.insert(f.Offset)
	}
	names.decode(dec)
	for _, f := range m.ParameterFields {
		f.FieldName = names.name(f.Offset)
	}
	return dec.Error()
}

// InputParameters represents the set of input paramters.
type InputParameters struct {
	InputFields []*ParameterField
	nvargs      []driver.NamedValue
	hasLob      bool
}

// NewInputParameters returns a InputParameters instance.
func NewInputParameters(inputFields []*ParameterField, nvargs []driver.NamedValue, hasLob bool) (*InputParameters, error) {
	return &InputParameters{InputFields: inputFields, nvargs: nvargs, hasLob: hasLob}, nil
}

func (p *InputParameters) String() string {
	return fmt.Sprintf("fields %s len(args) %d args %v", p.InputFields, len(p.nvargs), p.nvargs)
}

func (p *InputParameters) size() int {
	size := 0
	numColumns := len(p.InputFields)
	if numColumns == 0 { // avoid divide-by-zero (e.g. prepare without parameters)
		return 0
	}

	for i := 0; i < len(p.nvargs)/numColumns; i++ { // row-by-row

		size += numColumns

		for j := 0; j < numColumns; j++ {
			f := p.InputFields[j]
			size += f.prmSize(p.nvargs[i*numColumns+j].Value)
		}

		// lob input parameter: set offset position of lob data
		if p.hasLob {
			for j := 0; j < numColumns; j++ {
				if lobInDescr, ok := p.nvargs[i*numColumns+j].Value.(*LobInDescr); ok {
					lobInDescr.setPos(size)
					size += lobInDescr.size()
				}
			}
		}
	}
	return size
}

func (p *InputParameters) numArg() int {
	numColumns := len(p.InputFields)
	if numColumns == 0 { // avoid divide-by-zero (e.g. prepare without parameters)
		return 0
	}
	return len(p.nvargs) / numColumns
}

func (p *InputParameters) decode(dec *encoding.Decoder, ph *PartHeader) error {
	// TODO Sniffer
	//return fmt.Errorf("not implemented")
	return nil
}

func (p *InputParameters) encode(enc *encoding.Encoder) error {
	numColumns := len(p.InputFields)
	if numColumns == 0 { // avoid divide-by-zero (e.g. prepare without parameters)
		return nil
	}

	for i := 0; i < len(p.nvargs)/numColumns; i++ { // row-by-row
		for j := 0; j < numColumns; j++ {
			//mass insert
			f := p.InputFields[j]
			if err := f.encodePrm(enc, p.nvargs[i*numColumns+j].Value); err != nil {
				return err
			}
		}
		// lob input parameter: write first data chunk
		if p.hasLob {
			for j := 0; j < numColumns; j++ {
				if lobInDescr, ok := p.nvargs[i*numColumns+j].Value.(*LobInDescr); ok {
					lobInDescr.writeFirst(enc)
				}
			}
		}
	}
	return nil
}

// OutputParameters represents the set of output parameters.
type OutputParameters struct {
	OutputFields []*ParameterField
	FieldValues  []driver.Value
	DecodeErrors DecodeErrors
}

func (p *OutputParameters) String() string {
	return fmt.Sprintf("fields %v values %v", p.OutputFields, p.FieldValues)
}

func (p *OutputParameters) decode(dec *encoding.Decoder, ph *PartHeader) error {
	numArg := ph.numArg()
	cols := len(p.OutputFields)
	p.FieldValues = resizeFieldValues(p.FieldValues, numArg*cols)

	for i := 0; i < numArg; i++ {
		for j, f := range p.OutputFields {
			var err error
			if p.FieldValues[i*cols+j], err = f.decodeRes(dec); err != nil {
				p.DecodeErrors = append(p.DecodeErrors, &DecodeError{row: i, fieldName: f.Name(), s: err.Error()}) // collect decode / conversion errors
			}
		}
	}
	return dec.Error()
}
