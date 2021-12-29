package parquet

import (
	"fmt"
	"math"
	"reflect"
)

const (
	MaxRepetitionLevel = math.MaxInt8
	MaxDefinitionLevel = math.MaxInt8
	MaxColumnIndex     = math.MaxInt8
)

// Row represents a parquet row as a slice of values.
//
// Each value should embed a column index, repetition level, and definition
// level allowing the program to determine how to reconstruct the original
// object from the row. Repeated values share the same column index, their
// relative position of repeated values is represented by their relative
// position in the row.
type Row []Value

func (row Row) startsWith(columnIndex int) bool {
	return len(row) > 0 && int(row[0].ColumnIndex()) == columnIndex
}

// =============================================================================
// Functions returning closures are marked with "go:noinline" below to prevent
// losing naming information of the closure in stack traces.
//
// Because some of the functions are very short (simply return a closure), the
// compiler inlines when at their call site, which result in the closure being
// named something like parquet.deconstructFuncOf.func2 instead of the original
// parquet.deconstructFuncOfLeaf.func1; the latter being much more meanginful
// when reading CPU or memory profiles.
// =============================================================================

type levels struct {
	repetitionDepth int8
	repetitionLevel int8
	definitionLevel int8
}

type deconstructFunc func(Row, levels, reflect.Value) Row

func deconstructFuncOf(columnIndex int, node Node) (int, deconstructFunc) {
	switch {
	case node.Optional():
		return deconstructFuncOfOptional(columnIndex, node)
	case node.Repeated():
		return deconstructFuncOfRepeated(columnIndex, node)
	case isList(node):
		return deconstructFuncOfList(columnIndex, node)
	case isMap(node):
		return deconstructFuncOfMap(columnIndex, node)
	default:
		return deconstructFuncOfRequired(columnIndex, node)
	}
}

//go:noinline
func deconstructFuncOfOptional(columnIndex int, node Node) (int, deconstructFunc) {
	columnIndex, deconstruct := deconstructFuncOf(columnIndex, Required(node))
	return columnIndex, func(row Row, levels levels, value reflect.Value) Row {
		if value.IsValid() {
			if value.IsZero() {
				value = reflect.Value{}
			} else {
				if value.Kind() == reflect.Ptr {
					value = value.Elem()
				}
				levels.definitionLevel++
			}
		}
		return deconstruct(row, levels, value)
	}
}

//go:noinline
func deconstructFuncOfRepeated(columnIndex int, node Node) (int, deconstructFunc) {
	columnIndex, deconstruct := deconstructFuncOf(columnIndex, Required(node))
	return columnIndex, func(row Row, levels levels, value reflect.Value) Row {
		numValues := 0

		if value.IsValid() {
			numValues = value.Len()
			levels.repetitionDepth++
			if numValues > 0 {
				levels.definitionLevel++
			}
		}

		if numValues == 0 {
			return deconstruct(row, levels, reflect.Value{})
		}

		for i := 0; i < numValues; i++ {
			row = deconstruct(row, levels, value.Index(i))
			levels.repetitionLevel = levels.repetitionDepth
		}

		return row
	}
}

func deconstructFuncOfRequired(columnIndex int, node Node) (int, deconstructFunc) {
	switch {
	case isLeaf(node):
		return deconstructFuncOfLeaf(columnIndex, node)
	default:
		return deconstructFuncOfGroup(columnIndex, node)
	}
}

func deconstructFuncOfList(columnIndex int, node Node) (int, deconstructFunc) {
	return deconstructFuncOf(columnIndex, Repeated(listElementOf(node)))
}

//go:noinline
func deconstructFuncOfMap(columnIndex int, node Node) (int, deconstructFunc) {
	keyValue := mapKeyValueOf(node)
	keyValueType := keyValue.GoType()
	keyValueElem := keyValueType.Elem()
	keyType := keyValueElem.Field(0).Type
	valueType := keyValueElem.Field(1).Type
	columnIndex, deconstruct := deconstructFuncOf(columnIndex, Repeated(schemaOf(keyValueElem)))
	return columnIndex, func(row Row, levels levels, mapValue reflect.Value) Row {
		if !mapValue.IsValid() {
			return deconstruct(row, levels, reflect.Value{})
		}

		i := 0
		n := mapValue.Len()
		keyValueSlice := reflect.New(keyValueType).Elem()
		keyValueSlice.Set(reflect.MakeSlice(keyValueType, n, n))

		for _, mapKey := range mapValue.MapKeys() {
			keyValueElem := keyValueSlice.Index(i)
			keyValueElem.Field(0).Set(mapKey.Convert(keyType))
			keyValueElem.Field(1).Set(mapValue.MapIndex(mapKey).Convert(valueType))
			i++
		}

		return deconstruct(row, levels, keyValueSlice)
	}
}

//go:noinline
func deconstructFuncOfGroup(columnIndex int, node Node) (int, deconstructFunc) {
	names := node.ChildNames()
	funcs := make([]deconstructFunc, len(names))

	for i, name := range names {
		columnIndex, funcs[i] = deconstructFuncOf(columnIndex, node.ChildByName(name))
	}

	valueByIndex := func(value reflect.Value, index int) reflect.Value {
		return node.ValueByName(value, names[index])
	}

	switch n := unwrap(node).(type) {
	case IndexedNode:
		valueByIndex = n.ValueByIndex
	}

	return columnIndex, func(row Row, levels levels, value reflect.Value) Row {
		valueAt := valueByIndex

		if !value.IsValid() {
			valueAt = func(value reflect.Value, _ int) reflect.Value {
				return value
			}
		}

		for i, f := range funcs {
			row = f(row, levels, valueAt(value, i))
		}

		return row
	}
}

//go:noinline
func deconstructFuncOfLeaf(columnIndex int, node Node) (int, deconstructFunc) {
	if columnIndex > MaxColumnIndex {
		panic("row cannot be deconstructed because it has more than 127 columns")
	}
	kind := node.Type().Kind()
	valueColumnIndex := ^int8(columnIndex)
	return columnIndex + 1, func(row Row, levels levels, value reflect.Value) Row {
		v := Value{}

		if value.IsValid() {
			v = makeValue(kind, value)
		}

		v.repetitionLevel = levels.repetitionLevel
		v.definitionLevel = levels.definitionLevel
		v.columnIndex = valueColumnIndex
		return append(row, v)
	}
}

type reconstructFunc func(reflect.Value, levels, Row) (Row, error)

func reconstructFuncOf(columnIndex int, node Node) (int, reconstructFunc) {
	switch {
	case node.Optional():
		return reconstructFuncOfOptional(columnIndex, node)
	case node.Repeated():
		return reconstructFuncOfRepeated(columnIndex, node)
	case isList(node):
		return reconstructFuncOfList(columnIndex, node)
	case isMap(node):
		return reconstructFuncOfMap(columnIndex, node)
	default:
		return reconstructFuncOfRequired(columnIndex, node)
	}
}

//go:noinline
func reconstructFuncOfOptional(columnIndex int, node Node) (int, reconstructFunc) {
	nextColumnIndex, reconstruct := reconstructFuncOf(columnIndex, Required(node))
	rowLength := nextColumnIndex - columnIndex
	return nextColumnIndex, func(value reflect.Value, levels levels, row Row) (Row, error) {
		if !row.startsWith(columnIndex) {
			return row, fmt.Errorf("row is missing optional column %d", columnIndex)
		}
		if len(row) < rowLength {
			return row, fmt.Errorf("expected optional column %d to have at least %d values but got %d", columnIndex, rowLength, len(row))
		}

		levels.definitionLevel++

		if row[0].definitionLevel < levels.definitionLevel {
			value.Set(reflect.Zero(value.Type()))
			return row[rowLength:], nil
		}

		if value.Kind() == reflect.Ptr {
			if value.IsNil() {
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}

		return reconstruct(value, levels, row)
	}
}

//go:noinline
func reconstructFuncOfRepeated(columnIndex int, node Node) (int, reconstructFunc) {
	nextColumnIndex, reconstruct := reconstructFuncOf(columnIndex, Required(node))
	rowLength := nextColumnIndex - columnIndex
	return nextColumnIndex, func(value reflect.Value, levels levels, row Row) (Row, error) {
		if !row.startsWith(columnIndex) {
			return row, fmt.Errorf("row is missing repeated column %d", columnIndex)
		}
		if len(row) < rowLength {
			return row, fmt.Errorf("expected repeated column %d to have at least %d values but got %d", columnIndex, rowLength, len(row))
		}
		typ := value.Type()
		if typ.Kind() != reflect.Slice {
			return row, fmt.Errorf("cannot reconstruct repeated column %d into go value of type %s", columnIndex, value.Type())
		}

		levels.definitionLevel++
		levels.repetitionDepth++

		if row[0].definitionLevel < levels.definitionLevel {
			value.Set(reflect.MakeSlice(typ, 0, 0))
			return row[rowLength:], nil
		}

		n := 0
		c := value.Cap()
		if c > 0 {
			value.Set(value.Slice(0, c))
		} else {
			c = 10
			value.Set(reflect.MakeSlice(typ, c, c))
		}

		var err error
		for row.startsWith(columnIndex) && row[0].repetitionLevel == levels.repetitionLevel {
			if n == c {
				c *= 2
				newValue := reflect.MakeSlice(typ, c, c)
				reflect.Copy(newValue, value)
				value.Set(newValue)
			}

			if row, err = reconstruct(value.Index(n), levels, row); err != nil {
				return row, err
			}

			n++
			levels.repetitionLevel = levels.repetitionDepth
		}

		if n < c {
			value.Set(value.Slice(0, n))
		}

		return row, nil
	}
}

func reconstructFuncOfRequired(columnIndex int, node Node) (int, reconstructFunc) {
	switch {
	case isLeaf(node):
		return reconstructFuncOfLeaf(columnIndex, node)
	default:
		return reconstructFuncOfGroup(columnIndex, node)
	}
}

func reconstructFuncOfList(columnIndex int, node Node) (int, reconstructFunc) {
	return reconstructFuncOf(columnIndex, Repeated(listElementOf(node)))
}

//go:noinline
func reconstructFuncOfMap(columnIndex int, node Node) (int, reconstructFunc) {
	keyValue := mapKeyValueOf(node)
	keyValueType := keyValue.GoType()
	keyValueElem := keyValueType.Elem()
	columnIndex, reconstruct := reconstructFuncOf(columnIndex, Repeated(schemaOf(keyValueElem)))
	return columnIndex, func(mapValue reflect.Value, levels levels, row Row) (Row, error) {
		keyValueSlice := reflect.New(keyValueType).Elem()

		row, err := reconstruct(keyValueSlice, levels, row)
		if err != nil {
			return row, err
		}

		if mapType := mapValue.Type(); keyValueSlice.IsNil() {
			mapValue.Set(reflect.Zero(mapType))
		} else {
			keyType := mapType.Key()
			valueType := mapType.Elem()

			if mapValue.IsNil() {
				mapValue.Set(reflect.MakeMap(mapType))
			}

			for i, n := 0, keyValueSlice.Len(); i < n; i++ {
				elem := keyValueSlice.Index(i)
				mapValue.SetMapIndex(elem.Field(0).Convert(keyType), elem.Field(1).Convert(valueType))
			}
		}

		return row, nil
	}
}

//go:noinline
func reconstructFuncOfGroup(columnIndex int, node Node) (int, reconstructFunc) {
	names := node.ChildNames()
	funcs := make([]reconstructFunc, len(names))
	columnIndexes := make([]int, len(names))

	for i, name := range names {
		columnIndex, funcs[i] = reconstructFuncOf(columnIndex, node.ChildByName(name))
		columnIndexes[i] = columnIndex
	}

	valueByIndex := func(value reflect.Value, index int) reflect.Value {
		return node.ValueByName(value, names[index])
	}

	switch n := unwrap(node).(type) {
	case IndexedNode:
		valueByIndex = n.ValueByIndex
	}

	return columnIndex, func(value reflect.Value, levels levels, row Row) (Row, error) {
		var valueAt = valueByIndex
		var err error

		for i, f := range funcs {
			if row, err = f(valueAt(value, i), levels, row); err != nil {
				err = fmt.Errorf("%s → %w", names[i], err)
				break
			}
		}

		return row, err
	}
}

//go:noinline
func reconstructFuncOfLeaf(columnIndex int, node Node) (int, reconstructFunc) {
	return columnIndex + 1, func(value reflect.Value, _ levels, row Row) (Row, error) {
		if len(row) == 0 {
			return row, fmt.Errorf("expected one value to reconstruct leaf parquet row for column %d but found %d", columnIndex, len(row))
		}
		if int(row[0].ColumnIndex()) != columnIndex {
			return row, fmt.Errorf("no values found in parquet row for column %d", columnIndex)
		}
		return row[1:], assignValue(value, row[0])
	}
}
