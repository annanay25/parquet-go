package parquet

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/segmentio/parquet/deprecated"
	"github.com/segmentio/parquet/encoding"
)

// BufferColumn is an interface representing columns of a row group.
//
// BufferColumn implements sort.Interface as a way to support reordering the
// rows that have been written to it.
type BufferColumn interface {
	RowGroupColumn
	RowReaderAt
	RowWriter

	// Returns a copy of the column. The returned copy shares no memory with
	// the original, mutations of either column will not modify the other.
	Clone() BufferColumn

	// Clears all rows written to the column.
	Reset()

	// Returns the current capacity of the column (rows).
	Cap() int

	// Returns the number of rows currently written to the column.
	Len() int

	// Compares rows at index i and j and returns whether i < j.
	Less(i, j int) bool

	// Swaps rows at index i and j.
	Swap(i, j int)
}

type nullOrdering func(BufferColumn, int, int, int8, []int8) bool

func nullsGoFirst(column BufferColumn, i, j int, maxDefinitionLevel int8, definitionLevels []int8) bool {
	if isNull(i, maxDefinitionLevel, definitionLevels) {
		return !isNull(j, maxDefinitionLevel, definitionLevels)
	} else {
		return !isNull(j, maxDefinitionLevel, definitionLevels) && column.Less(i, j)
	}
}

func nullsGoLast(column BufferColumn, i, j int, maxDefinitionLevel int8, definitionLevels []int8) bool {
	if isNull(i, maxDefinitionLevel, definitionLevels) {
		return false
	} else {
		return isNull(j, maxDefinitionLevel, definitionLevels) || column.Less(i, j)
	}
}

func isNull(i int, maxDefinitionLevel int8, definitionLevels []int8) bool {
	return definitionLevels[i] != maxDefinitionLevel
}

func rowGroupColumnPageWithoutNulls(column BufferColumn, maxDefinitionLevel int8, definitionLevels []int8) Page {
	n := 0
	for i := 0; i < len(definitionLevels); {
		j := i
		for j < len(definitionLevels) && isNull(j, maxDefinitionLevel, definitionLevels) {
			j++
		}
		if j < len(definitionLevels) {
			column.Swap(n, j)
			n++
		}
		i = j + 1
	}
	return column.Page().Slice(0, n)
}

type reversedBufferColumn struct{ BufferColumn }

func (col *reversedBufferColumn) Less(i, j int) bool { return col.BufferColumn.Less(j, i) }

type optionalBufferColumn struct {
	base               BufferColumn
	maxDefinitionLevel int8
	definitionLevels   []int8
	nullOrdering       nullOrdering
}

func newOptionalBufferColumn(base BufferColumn, maxDefinitionLevel int8, nullOrdering nullOrdering) *optionalBufferColumn {
	return &optionalBufferColumn{
		base:               base,
		maxDefinitionLevel: maxDefinitionLevel,
		definitionLevels:   make([]int8, 0, base.Cap()),
		nullOrdering:       nullOrdering,
	}
}

func (col *optionalBufferColumn) Clone() BufferColumn {
	return &optionalBufferColumn{
		base:               col.base.Clone(),
		maxDefinitionLevel: col.maxDefinitionLevel,
		definitionLevels:   append([]int8{}, col.definitionLevels...),
		nullOrdering:       col.nullOrdering,
	}
}

func (col *optionalBufferColumn) Dictionary() Dictionary {
	return col.base.Dictionary()
}

func (col *optionalBufferColumn) Page() Page {
	return newOptionalPage(
		rowGroupColumnPageWithoutNulls(col.base, col.maxDefinitionLevel, col.definitionLevels),
		col.maxDefinitionLevel,
		col.definitionLevels,
	)
}

func (col *optionalBufferColumn) Reset() {
	col.base.Reset()
	col.definitionLevels = col.definitionLevels[:0]
}

func (col *optionalBufferColumn) Size() int64 {
	return col.base.Size() + int64(len(col.definitionLevels))
}

func (col *optionalBufferColumn) Cap() int { return cap(col.definitionLevels) }

func (col *optionalBufferColumn) Len() int { return len(col.definitionLevels) }

func (col *optionalBufferColumn) Less(i, j int) bool {
	return col.nullOrdering(col.base, i, j, col.maxDefinitionLevel, col.definitionLevels)
}

func (col *optionalBufferColumn) Swap(i, j int) {
	col.base.Swap(i, j)
	col.definitionLevels[i], col.definitionLevels[j] = col.definitionLevels[j], col.definitionLevels[i]
}

func (col *optionalBufferColumn) WriteRow(row Row) error {
	if err := col.base.WriteRow(row); err != nil {
		return err
	}
	for _, v := range row {
		col.definitionLevels = append(col.definitionLevels, v.DefinitionLevel())
	}
	return nil
}

func (col *optionalBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	if index < 0 {
		return row, errRowIndexOutOfBounds(index, len(col.definitionLevels))
	}
	if index >= len(col.definitionLevels) {
		return row, io.EOF
	}

	if definitionLevel := col.definitionLevels[index]; definitionLevel != col.maxDefinitionLevel {
		row = append(row, Value{definitionLevel: definitionLevel})
	} else {
		var err error
		var n = len(row)

		if row, err = col.base.ReadRowAt(row, index); err != nil {
			return row, err
		}
		if n == len(row) {
			return row[:n], fmt.Errorf("optional column has no value for row index %d", index)
		}
		if n != (len(row) - 1) {
			return row[:n], fmt.Errorf("optional column has more than one value for row index %d", index)
		}

		row[n].definitionLevel = definitionLevel
	}

	return row, nil
}

type repeatedBufferColumn struct {
	base               BufferColumn
	maxRepetitionLevel int8
	maxDefinitionLevel int8
	rows               []region
	repetitionLevels   []int8
	definitionLevels   []int8
	buffer             []Value
	reordering         *repeatedBufferColumn
	nullOrdering       nullOrdering
}

type region struct {
	offset uint32
	length uint32
}

func newRepeatedBufferColumn(base BufferColumn, maxRepetitionLevel, maxDefinitionLevel int8, nullOrdering nullOrdering) *repeatedBufferColumn {
	n := base.Cap()
	return &repeatedBufferColumn{
		base:               base,
		maxRepetitionLevel: maxRepetitionLevel,
		maxDefinitionLevel: maxDefinitionLevel,
		rows:               make([]region, 0, n/8),
		repetitionLevels:   make([]int8, 0, n),
		definitionLevels:   make([]int8, 0, n),
		nullOrdering:       nullOrdering,
	}
}

func rowsHaveBeenReordered(rows []region) bool {
	offset := uint32(0)
	for _, row := range rows {
		if row.offset != offset {
			return true
		}
		offset += row.length
	}
	return false
}

func maxRowLengthOf(rows []region) (maxLength uint32) {
	for _, row := range rows {
		if row.length > maxLength {
			maxLength = row.length
		}
	}
	return maxLength
}

func (col *repeatedBufferColumn) Clone() BufferColumn {
	return &repeatedBufferColumn{
		base:               col.base.Clone(),
		maxRepetitionLevel: col.maxRepetitionLevel,
		maxDefinitionLevel: col.maxDefinitionLevel,
		rows:               append([]region{}, col.rows...),
		repetitionLevels:   append([]int8{}, col.repetitionLevels...),
		definitionLevels:   append([]int8{}, col.definitionLevels...),
		nullOrdering:       col.nullOrdering,
	}
}

func (col *repeatedBufferColumn) Dictionary() Dictionary {
	return col.base.Dictionary()
}

func (col *repeatedBufferColumn) Page() Page {
	base := col.base
	repetitionLevels := col.repetitionLevels
	definitionLevels := col.definitionLevels

	if rowsHaveBeenReordered(col.rows) {
		if col.reordering == nil {
			col.reordering = col.Clone().(*repeatedBufferColumn)
		}

		maxLen := maxRowLengthOf(col.rows)
		if maxLen > uint32(cap(col.buffer)) {
			col.buffer = make([]Value, maxLen)
		}

		column := col.reordering
		buffer := col.buffer[:maxLen]
		page := base.Page()
		column.Reset()

		for _, row := range col.rows {
			values := buffer[:row.length]
			n, err := page.ReadValuesAt(int(row.offset), values)
			if err != nil && n < len(values) {
				return &errorPage{err: fmt.Errorf("reordering values of repeated column: %w", err)}
			}
			if err := column.base.WriteRow(values); err != nil {
				return &errorPage{err: fmt.Errorf("reordering values of repeated column: %w", err)}
			}
		}

		for _, row := range col.rows {
			column.rows = append(column.rows, column.row(int(row.length)))
			column.repetitionLevels = append(column.repetitionLevels, col.repetitionLevels[row.offset:row.offset+row.length]...)
			column.definitionLevels = append(column.definitionLevels, col.definitionLevels[row.offset:row.offset+row.length]...)
		}

		base = column.base
		repetitionLevels = column.repetitionLevels
		definitionLevels = column.definitionLevels
	}

	return newRepeatedPage(
		rowGroupColumnPageWithoutNulls(base, col.maxDefinitionLevel, definitionLevels),
		col.maxRepetitionLevel,
		col.maxDefinitionLevel,
		repetitionLevels,
		definitionLevels,
	)
}

func (col *repeatedBufferColumn) Reset() {
	col.base.Reset()
	col.rows = col.rows[:0]
	col.repetitionLevels = col.repetitionLevels[:0]
	col.definitionLevels = col.definitionLevels[:0]
}

func (col *repeatedBufferColumn) Size() int64 {
	return 8*int64(len(col.rows)) + int64(len(col.repetitionLevels)) + int64(len(col.definitionLevels)) + col.base.Size()
}

func (col *repeatedBufferColumn) Cap() int { return cap(col.rows) }

func (col *repeatedBufferColumn) Len() int { return len(col.rows) }

func (col *repeatedBufferColumn) Less(i, j int) bool {
	row1 := col.rows[i]
	row2 := col.rows[j]
	less := col.nullOrdering

	for k := uint32(0); k < row1.length && k < row2.length; k++ {
		x := int(row1.offset + k)
		y := int(row2.offset + k)
		switch {
		case less(col.base, x, y, col.maxDefinitionLevel, col.definitionLevels):
			return true
		case less(col.base, y, x, col.maxDefinitionLevel, col.definitionLevels):
			return false
		}
	}

	return row1.length < row2.length
}

func (col *repeatedBufferColumn) Swap(i, j int) {
	col.rows[i], col.rows[j] = col.rows[j], col.rows[i]
}

func (col *repeatedBufferColumn) WriteRow(row Row) error {
	if err := col.base.WriteRow(row); err != nil {
		return err
	}
	col.rows = append(col.rows, col.row(len(row)))
	for _, v := range row {
		col.repetitionLevels = append(col.repetitionLevels, v.RepetitionLevel())
		col.definitionLevels = append(col.definitionLevels, v.DefinitionLevel())
	}
	return nil
}

func (col *repeatedBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	if index < 0 {
		return row, errRowIndexOutOfBounds(index, len(col.rows))
	}
	if index >= len(col.rows) {
		return row, io.EOF
	}

	reset := len(row)
	region := col.rows[index]
	maxDefinitionLevel := col.maxDefinitionLevel
	repetitionLevels := col.repetitionLevels[region.offset : region.offset+region.length]
	definitionLevels := col.definitionLevels[region.offset : region.offset+region.length]

	for i := range definitionLevels {
		if definitionLevels[i] != maxDefinitionLevel {
			row = append(row, Value{
				repetitionLevel: repetitionLevels[i],
				definitionLevel: definitionLevels[i],
			})
		} else {
			var err error
			var n = len(row)

			if row, err = col.base.ReadRowAt(row, int(region.offset)+i); err != nil {
				return row[:reset], err
			}
			if n == len(row) {
				return row[:reset], fmt.Errorf("repeated column has no values for element %d of row index %d", i, index)
			}
			if n != (len(row) - 1) {
				return row[:reset], fmt.Errorf("repeated column has more than one value for element %d of row index %d", i, index)
			}

			row[n].repetitionLevel = repetitionLevels[i]
			row[n].definitionLevel = definitionLevels[i]
		}
	}

	return row, nil
}

func (col *repeatedBufferColumn) row(n int) region {
	return region{
		offset: uint32(len(col.repetitionLevels)),
		length: uint32(n),
	}
}

type booleanBufferColumn struct{ booleanPage }

func newBooleanBufferColumn(bufferSize int) *booleanBufferColumn {
	return &booleanBufferColumn{
		booleanPage: booleanPage{
			values: make([]bool, 0, bufferSize),
		},
	}
}

func (col *booleanBufferColumn) Clone() BufferColumn {
	return &booleanBufferColumn{
		booleanPage: booleanPage{
			values: append([]bool{}, col.values...),
		},
	}
}

func (col *booleanBufferColumn) Dictionary() Dictionary { return nil }

func (col *booleanBufferColumn) Page() Page { return &col.booleanPage }

func (col *booleanBufferColumn) Reset() { col.values = col.values[:0] }

func (col *booleanBufferColumn) Size() int64 { return int64(len(col.values)) }

func (col *booleanBufferColumn) Cap() int { return cap(col.values) }

func (col *booleanBufferColumn) Len() int { return len(col.values) }

func (col *booleanBufferColumn) Less(i, j int) bool {
	return col.values[i] != col.values[j] && !col.values[i]
}

func (col *booleanBufferColumn) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *booleanBufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values = append(col.values, v.Boolean())
	}
	return nil
}

func (col *booleanBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, len(col.values))
	case index >= len(col.values):
		return row, io.EOF
	default:
		return append(row, makeValueBoolean(col.values[index])), nil
	}
}

type int32BufferColumn struct{ int32Page }

func newInt32BufferColumn(bufferSize int) *int32BufferColumn {
	return &int32BufferColumn{
		int32Page: int32Page{
			values: make([]int32, 0, bufferSize/4),
		},
	}
}

func (col *int32BufferColumn) Clone() BufferColumn {
	return &int32BufferColumn{
		int32Page: int32Page{
			values: append([]int32{}, col.values...),
		},
	}
}

func (col *int32BufferColumn) Dictionary() Dictionary { return nil }

func (col *int32BufferColumn) Page() Page { return &col.int32Page }

func (col *int32BufferColumn) Reset() { col.values = col.values[:0] }

func (col *int32BufferColumn) Size() int64 { return 4 * int64(len(col.values)) }

func (col *int32BufferColumn) Cap() int { return cap(col.values) }

func (col *int32BufferColumn) Len() int { return len(col.values) }

func (col *int32BufferColumn) Less(i, j int) bool { return col.values[i] < col.values[j] }

func (col *int32BufferColumn) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *int32BufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values = append(col.values, v.Int32())
	}
	return nil
}

func (col *int32BufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, len(col.values))
	case index >= len(col.values):
		return row, io.EOF
	default:
		return append(row, makeValueInt32(col.values[index])), nil
	}
}

type int64BufferColumn struct{ int64Page }

func newInt64BufferColumn(bufferSize int) *int64BufferColumn {
	return &int64BufferColumn{
		int64Page: int64Page{
			values: make([]int64, 0, bufferSize/8),
		},
	}
}

func (col *int64BufferColumn) Clone() BufferColumn {
	return &int64BufferColumn{
		int64Page: int64Page{
			values: append([]int64{}, col.values...),
		},
	}
}

func (col *int64BufferColumn) Dictionary() Dictionary { return nil }

func (col *int64BufferColumn) Page() Page { return &col.int64Page }

func (col *int64BufferColumn) Reset() { col.values = col.values[:0] }

func (col *int64BufferColumn) Size() int64 { return 8 * int64(len(col.values)) }

func (col *int64BufferColumn) Cap() int { return cap(col.values) }

func (col *int64BufferColumn) Len() int { return len(col.values) }

func (col *int64BufferColumn) Less(i, j int) bool { return col.values[i] < col.values[j] }

func (col *int64BufferColumn) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *int64BufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values = append(col.values, v.Int64())
	}
	return nil
}

func (col *int64BufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, len(col.values))
	case index >= len(col.values):
		return row, io.EOF
	default:
		return append(row, makeValueInt64(col.values[index])), nil
	}
}

type int96BufferColumn struct{ int96Page }

func newInt96BufferColumn(bufferSize int) *int96BufferColumn {
	return &int96BufferColumn{
		int96Page: int96Page{
			values: make([]deprecated.Int96, 0, bufferSize/12),
		},
	}
}

func (col *int96BufferColumn) Clone() BufferColumn {
	return &int96BufferColumn{
		int96Page: int96Page{
			values: append([]deprecated.Int96{}, col.values...),
		},
	}
}

func (col *int96BufferColumn) Dictionary() Dictionary { return nil }

func (col *int96BufferColumn) Page() Page { return &col.int96Page }

func (col *int96BufferColumn) Reset() { col.values = col.values[:0] }

func (col *int96BufferColumn) Size() int64 { return 12 * int64(len(col.values)) }

func (col *int96BufferColumn) Cap() int { return cap(col.values) }

func (col *int96BufferColumn) Len() int { return len(col.values) }

func (col *int96BufferColumn) Less(i, j int) bool { return col.values[i].Less(col.values[j]) }

func (col *int96BufferColumn) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *int96BufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values = append(col.values, v.Int96())
	}
	return nil
}

func (col *int96BufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, len(col.values))
	case index >= len(col.values):
		return row, io.EOF
	default:
		return append(row, makeValueInt96(col.values[index])), nil
	}
}

type floatBufferColumn struct{ floatPage }

func newFloatBufferColumn(bufferSize int) *floatBufferColumn {
	return &floatBufferColumn{
		floatPage: floatPage{
			values: make([]float32, 0, bufferSize/4),
		},
	}
}

func (col *floatBufferColumn) Clone() BufferColumn {
	return &floatBufferColumn{
		floatPage: floatPage{
			values: append([]float32{}, col.values...),
		},
	}
}

func (col *floatBufferColumn) Dictionary() Dictionary { return nil }

func (col *floatBufferColumn) Page() Page { return &col.floatPage }

func (col *floatBufferColumn) Reset() { col.values = col.values[:0] }

func (col *floatBufferColumn) Size() int64 { return 4 * int64(len(col.values)) }

func (col *floatBufferColumn) Cap() int { return cap(col.values) }

func (col *floatBufferColumn) Len() int { return len(col.values) }

func (col *floatBufferColumn) Less(i, j int) bool { return col.values[i] < col.values[j] }

func (col *floatBufferColumn) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *floatBufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values = append(col.values, v.Float())
	}
	return nil
}

func (col *floatBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, len(col.values))
	case index >= len(col.values):
		return row, io.EOF
	default:
		return append(row, makeValueFloat(col.values[index])), nil
	}
}

type doubleBufferColumn struct{ doublePage }

func newDoubleBufferColumn(bufferSize int) *doubleBufferColumn {
	return &doubleBufferColumn{
		doublePage: doublePage{
			values: make([]float64, 0, bufferSize/8),
		},
	}
}

func (col *doubleBufferColumn) Clone() BufferColumn {
	return &doubleBufferColumn{
		doublePage: doublePage{
			values: append([]float64{}, col.values...),
		},
	}
}

func (col *doubleBufferColumn) Dictionary() Dictionary { return nil }

func (col *doubleBufferColumn) Page() Page { return &col.doublePage }

func (col *doubleBufferColumn) Reset() { col.values = col.values[:0] }

func (col *doubleBufferColumn) Size() int64 { return 8 * int64(len(col.values)) }

func (col *doubleBufferColumn) Cap() int { return cap(col.values) }

func (col *doubleBufferColumn) Len() int { return len(col.values) }

func (col *doubleBufferColumn) Less(i, j int) bool { return col.values[i] < col.values[j] }

func (col *doubleBufferColumn) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *doubleBufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values = append(col.values, v.Double())
	}
	return nil
}

func (col *doubleBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, len(col.values))
	case index >= len(col.values):
		return row, io.EOF
	default:
		return append(row, makeValueDouble(col.values[index])), nil
	}
}

type byteArrayBufferColumn struct{ byteArrayPage }

func newByteArrayBufferColumn(bufferSize int) *byteArrayBufferColumn {
	return &byteArrayBufferColumn{
		byteArrayPage: byteArrayPage{
			values: encoding.MakeByteArrayList(bufferSize / 16),
		},
	}
}

func (col *byteArrayBufferColumn) Clone() BufferColumn {
	return &byteArrayBufferColumn{
		byteArrayPage: byteArrayPage{
			values: col.values.Clone(),
		},
	}
}

func (col *byteArrayBufferColumn) Dictionary() Dictionary { return nil }

func (col *byteArrayBufferColumn) Page() Page { return &col.byteArrayPage }

func (col *byteArrayBufferColumn) Reset() { col.values.Reset() }

func (col *byteArrayBufferColumn) Size() int64 { return col.values.Size() }

func (col *byteArrayBufferColumn) Cap() int { return col.values.Cap() }

func (col *byteArrayBufferColumn) Len() int { return col.values.Len() }

func (col *byteArrayBufferColumn) Less(i, j int) bool { return col.values.Less(i, j) }

func (col *byteArrayBufferColumn) Swap(i, j int) { col.values.Swap(i, j) }

func (col *byteArrayBufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.values.Push(v.ByteArray())
	}
	return nil
}

func (col *byteArrayBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, col.values.Len())
	case index >= col.values.Len():
		return row, io.EOF
	default:
		return append(row, makeValueBytes(ByteArray, col.values.Index(index))), nil
	}
}

type fixedLenByteArrayBufferColumn struct {
	fixedLenByteArrayPage
	tmp []byte
}

func newFixedLenByteArrayBufferColumn(size, bufferSize int) *fixedLenByteArrayBufferColumn {
	return &fixedLenByteArrayBufferColumn{
		fixedLenByteArrayPage: fixedLenByteArrayPage{
			size: size,
			data: make([]byte, 0, bufferSize),
		},
		tmp: make([]byte, size),
	}
}

func (col *fixedLenByteArrayBufferColumn) Clone() BufferColumn {
	return &fixedLenByteArrayBufferColumn{
		fixedLenByteArrayPage: fixedLenByteArrayPage{
			size: col.size,
			data: append([]byte{}, col.data...),
		},
		tmp: make([]byte, col.size),
	}
}

func (col *fixedLenByteArrayBufferColumn) Dictionary() Dictionary { return nil }

func (col *fixedLenByteArrayBufferColumn) Page() Page { return &col.fixedLenByteArrayPage }

func (col *fixedLenByteArrayBufferColumn) Reset() { col.data = col.data[:0] }

func (col *fixedLenByteArrayBufferColumn) Size() int64 { return int64(len(col.data)) }

func (col *fixedLenByteArrayBufferColumn) Cap() int { return cap(col.data) / col.size }

func (col *fixedLenByteArrayBufferColumn) Len() int { return len(col.data) / col.size }

func (col *fixedLenByteArrayBufferColumn) Less(i, j int) bool {
	return bytes.Compare(col.index(i), col.index(j)) < 0
}

func (col *fixedLenByteArrayBufferColumn) Swap(i, j int) {
	t, u, v := col.tmp[:col.size], col.index(i), col.index(j)
	copy(t, u)
	copy(u, v)
	copy(v, t)
}

func (col *fixedLenByteArrayBufferColumn) index(i int) []byte {
	j := (i + 0) * col.size
	k := (i + 1) * col.size
	return col.data[j:k:k]
}

func (col *fixedLenByteArrayBufferColumn) WriteRow(row Row) error {
	for _, v := range row {
		col.data = append(col.data, v.ByteArray()...)
	}
	return nil
}

func (col *fixedLenByteArrayBufferColumn) ReadRowAt(row Row, index int) (Row, error) {
	i := (index + 0) * col.size
	j := (index + 1) * col.size
	switch {
	case i < 0:
		return row, errRowIndexOutOfBounds(index, col.Len())
	case j > len(col.data):
		return row, io.EOF
	default:
		return append(row, makeValueBytes(FixedLenByteArray, col.data[i:j])), nil
	}
}

type uint32BufferColumn struct{ *int32BufferColumn }

func newUint32BufferColumn(bufferSize int) uint32BufferColumn {
	return uint32BufferColumn{newInt32BufferColumn(bufferSize)}
}

func (col uint32BufferColumn) Clone() BufferColumn {
	return uint32BufferColumn{col.int32BufferColumn.Clone().(*int32BufferColumn)}
}

func (col uint32BufferColumn) Page() Page {
	return uint32Page{&col.int32Page}
}

func (col uint32BufferColumn) Less(i, j int) bool {
	return uint32(col.values[i]) < uint32(col.values[j])
}

type uint64BufferColumn struct{ *int64BufferColumn }

func newUint64BufferColumn(bufferSize int) uint64BufferColumn {
	return uint64BufferColumn{newInt64BufferColumn(bufferSize)}
}

func (col uint64BufferColumn) Clone() BufferColumn {
	return uint64BufferColumn{col.int64BufferColumn.Clone().(*int64BufferColumn)}
}

func (col uint64BufferColumn) Page() Page {
	return uint64Page{&col.int64Page}
}

func (col uint64BufferColumn) Less(i, j int) bool {
	return uint64(col.values[i]) < uint64(col.values[j])
}

var (
	_ sort.Interface = (BufferColumn)(nil)
)
