package ptable

import (
	"encoding/binary"
	"math"
	"math/bits"
	"unsafe"
)

// Block layout
//
// +--------------------------------------------------------------------------+
// | ncols(4) | nrows(4) | page1(4) | page2(4) | ...    | pageN(4)            |
// +--------------------------------------------------------------------------+
// | <bool>  | null-bitmap | rank-lut | value-bitmap                          |
// +--------------------------------------------------------------------------+
// | <int32> | null-bitmap | rank-lut | values (4-byte aligned)               |
// +--------------------------------------------------------------------------+
// | <bytes> | null-bitmap | rank-lut | val1 | val2 | ... | pos (4) | pos (4) |
// +--------------------------------------------------------------------------+
// | ...                                                                      |
// +--------------------------------------------------------------------------+
//
// Blocks contain rows following a fixed schema. The data is stored in a column
// layout: all of the values for a column is stored contiguously. Column types
// have either fixed-width values, or variable-width. All variable-width values
// are stored in the "bytes" column type and it is up to higher levels to
// interpret.
//
// The data for a column is stored within a "page". The first byte in a page
// specifies the column type. Fixed width pages are then followed by a null
// bitmap with 1-bit per row indicating whether the column at that row is null
// or not. Following the null bitmap is the column data itself. The data is
// aligned to the required alignment of the column type (4 for int32, 8 for
// int64, etc) so that it can be accessed directly without decoding.
//
// The null-bitmap indicates the presence of a column value. If the i'th bit of
// the null-bitmap for a column is 1, no value is stored for the column at that
// index. A rank-lut (rank lookup table) is stored after the null-bitmap which
// allows fast determination of the "real" index for a non-NULL value. Rank(i)
// returns the index in the value array. If no column values are NULL, rank(i)
// == i. The rank lookup table is composed of rows/64 16-bit values where each
// value is the count of the non-NULL in the previous entries. Computing
// rank(i) involves a lookup in the rank-lut combined with a pop-count for the
// 64-bit chunk of the null-bitmap that i resides in.
//
// Variable width data (i.e. the "bytes" column type) is stored in a different
// format. Immediately following the column type are the concatenated variable
// length values. After the concatenated data is an array of offsets indicating
// the end of each column value within the concatenated data. For example,
// offset[0] is the end of the first row's column data. A negative offset
// indicates a null value.

type columnWriter struct {
	ctype   ColumnType
	data    []byte
	offsets []int32
	nulls   Bitmap
	count   int32
}

func (w *columnWriter) reset() {
	w.data = w.data[:0]
	w.offsets = w.offsets[:0]
	w.nulls = w.nulls[:0]
	w.count = 0
}

func (w *columnWriter) grow(n int) []byte {
	i := len(w.data)
	if cap(w.data)-i < n {
		newSize := 2 * cap(w.data)
		if newSize == 0 {
			newSize = 256
		}
		newData := make([]byte, i, newSize)
		copy(newData, w.data)
		w.data = newData
	}
	w.data = w.data[:i+n]
	return w.data[i:]
}

func (w *columnWriter) putBool(v bool) {
	if w.ctype != ColumnTypeBool {
		panic("bool column value expected")
	}
	w.data = (Bitmap)(w.data).set(int(w.count), v)
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putInt8(v int8) {
	if w.ctype != ColumnTypeInt8 {
		panic("int8 column value expected")
	}
	w.data = append(w.data, byte(v))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putInt16(v int16) {
	if w.ctype != ColumnTypeInt16 {
		panic("int16 column value expected")
	}
	binary.LittleEndian.PutUint16(w.grow(2), uint16(v))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putInt32(v int32) {
	if w.ctype != ColumnTypeInt32 {
		panic("int32 column value expected")
	}
	binary.LittleEndian.PutUint32(w.grow(4), uint32(v))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putInt64(v int64) {
	if w.ctype != ColumnTypeInt64 {
		panic("int64 column value expected")
	}
	binary.LittleEndian.PutUint64(w.grow(8), uint64(v))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putFloat32(v float32) {
	if w.ctype != ColumnTypeFloat32 {
		panic("float32 column value expected")
	}
	binary.LittleEndian.PutUint32(w.grow(4), math.Float32bits(v))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putFloat64(v float64) {
	if w.ctype != ColumnTypeFloat64 {
		panic("float64 column value expected")
	}
	binary.LittleEndian.PutUint64(w.grow(8), math.Float64bits(v))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putBytes(v []byte) {
	if w.ctype != ColumnTypeBytes {
		panic("bytes column value expected")
	}
	w.data = append(w.data, v...)
	w.offsets = append(w.offsets, int32(len(w.data)))
	w.nulls = w.nulls.set(int(w.count), false)
	w.count++
}

func (w *columnWriter) putNull() {
	w.nulls = w.nulls.set(int(w.count), true)
	if w.ctype.Width() <= 0 {
		w.offsets = append(w.offsets, int32(len(w.data)))
	}
	w.count++
}

func align(offset, val int32) int32 {
	return (offset + val - 1) & ^(val - 1)
}

func (w *columnWriter) encode(offset int32, buf []byte) int32 {
	// The column type.
	buf[offset] = byte(w.ctype)
	offset++
	// The null-bitmap.
	offset += int32(copy(buf[offset:], w.nulls))
	// The rank lookup table.
	offset = align(offset, 2)
	// The first entry of the rank-lut is always 0.
	binary.LittleEndian.PutUint16(buf[offset:], 0)
	offset += 2
	for i, sum := int32(64), uint16(0); i < w.count; i += 64 {
		// Add the present values for the previous 64-bit block of the null-bitmap.
		j := (i - 64) / 64
		sum += uint16(bits.OnesCount64(^binary.BigEndian.Uint64(w.nulls[j:])))
		binary.LittleEndian.PutUint16(buf[offset:], sum)
		offset += 2
	}
	// The column values.
	offset = align(offset, w.ctype.Alignment())
	offset += int32(copy(buf[offset:], w.data))
	// The offsets for variable width data.
	if w.ctype.Width() <= 0 {
		offset = align(offset, 4)
		dest := (*[1 << 31]int32)(unsafe.Pointer(&buf[offset]))[:w.count:w.count]
		copy(dest, w.offsets)
		offset += int32(len(w.offsets) * 4)
	}
	return offset
}

func (w *columnWriter) size(offset int32) int32 {
	startOffset := offset
	// The column type.
	offset++
	// The null-bitmap.
	offset += int32(len(w.nulls))
	// The rank lookup table.
	offset = align(offset, 2)
	offset += 2 * (int32(w.count+63) / 64)
	// The column values.
	offset = align(offset, w.ctype.Alignment())
	offset += int32(len(w.data))
	// The offsets for variable width data.
	if w.ctype.Width() <= 0 {
		offset = align(offset, 4)
		offset += int32(len(w.offsets) * 4)
	}
	return offset - startOffset
}

func blockHeaderSize(n int) int32 {
	return int32(8 + n*4)
}

func pageOffsetPos(i int) int32 {
	return int32(8 + i*4)
}

type blockWriter struct {
	cols []columnWriter
	buf  []byte
}

func (w *blockWriter) init(s []ColumnType) {
	w.cols = make([]columnWriter, len(s))
	for i := range w.cols {
		w.cols[i].ctype = s[i]
	}
}

func (w *blockWriter) reset() {
	for i := range w.cols {
		w.cols[i].reset()
	}
}

func (w *blockWriter) Finish() []byte {
	size := w.Size()
	if int32(cap(w.buf)) < size {
		w.buf = make([]byte, size)
	}
	w.buf = w.buf[:size]
	n := len(w.cols)
	binary.LittleEndian.PutUint32(w.buf[0:], uint32(n))
	binary.LittleEndian.PutUint32(w.buf[4:], uint32(w.cols[0].count))
	pageOffset := blockHeaderSize(n)
	for i := range w.cols {
		col := &w.cols[i]
		binary.LittleEndian.PutUint32(w.buf[pageOffsetPos(i):], uint32(pageOffset))
		pageOffset = col.encode(pageOffset, w.buf)
	}
	return w.buf
}

func (w *blockWriter) Size() int32 {
	size := blockHeaderSize(len(w.cols))
	for i := range w.cols {
		size += w.cols[i].size(size)
	}
	return size
}

func (w *blockWriter) PutRow(row RowReader) {
	for i := range w.cols {
		col := &w.cols[i]
		if row.Null(i) {
			col.putNull()
			continue
		}
		switch w.cols[i].ctype {
		case ColumnTypeBool:
			col.putBool(row.Bool(i))
		case ColumnTypeInt8:
			col.putInt8(row.Int8(i))
		case ColumnTypeInt16:
			col.putInt16(row.Int16(i))
		case ColumnTypeInt32:
			col.putInt32(row.Int32(i))
		case ColumnTypeInt64:
			col.putInt64(row.Int64(i))
		case ColumnTypeFloat32:
			col.putFloat32(row.Float32(i))
		case ColumnTypeFloat64:
			col.putFloat64(row.Float64(i))
		case ColumnTypeBytes:
			col.putBytes(row.Bytes(i))
		}
	}
}

func (w *blockWriter) PutBool(col int, v bool) {
	w.cols[col].putBool(v)
}

func (w *blockWriter) PutInt8(col int, v int8) {
	w.cols[col].putInt8(v)
}

func (w *blockWriter) PutInt16(col int, v int16) {
	w.cols[col].putInt16(v)
}

func (w *blockWriter) PutInt32(col int, v int32) {
	w.cols[col].putInt32(v)
}

func (w *blockWriter) PutInt64(col int, v int64) {
	w.cols[col].putInt64(v)
}

func (w *blockWriter) PutFloat32(col int, v float32) {
	w.cols[col].putFloat32(v)
}

func (w *blockWriter) PutFloat64(col int, v float64) {
	w.cols[col].putFloat64(v)
}

func (w *blockWriter) PutBytes(col int, v []byte) {
	w.cols[col].putBytes(v)
}

func (w *blockWriter) PutNull(col int) {
	w.cols[col].putNull()
}

type blockReader struct {
	start unsafe.Pointer
	len   int32
	cols  int32
	rows  int32
}

func newReader(data []byte) *blockReader {
	r := &blockReader{}
	r.init(data)
	return r
}

func (r *blockReader) init(data []byte) {
	r.start = unsafe.Pointer(&data[0])
	r.len = int32(len(data))
	r.cols = int32(binary.LittleEndian.Uint32(data[0:]))
	r.rows = int32(binary.LittleEndian.Uint32(data[4:]))
}

func (r *blockReader) pageStart(col int) int32 {
	if int32(col) >= r.cols {
		return r.len
	}
	return *(*int32)(unsafe.Pointer(uintptr(r.start) + 8 + uintptr(col*4)))
}

func (r *blockReader) pointer(offset int32) unsafe.Pointer {
	return unsafe.Pointer(uintptr(r.start) + uintptr(offset))
}

func (r *blockReader) Data() []byte {
	return (*[1 << 31]byte)(r.start)[:r.len:r.len]
}

func (r *blockReader) Column(col int) Vec {
	if col < 0 || int32(col) >= r.cols {
		panic("invalid column")
	}

	start := r.pageStart(col)
	data := r.pointer(start)

	v := Vec{N: r.rows}
	// The column type.
	v.Type = *(*ColumnType)(data)
	start++
	// The null bitmap.
	n := int32(r.rows+7) / 8
	v.Nulls = Bitmap((*[1 << 31]byte)(r.pointer(start + 1))[:n:n])
	start += n
	// The rank lookup table.
	start = align(start, 2)
	v.rank = r.pointer(start)
	start += 2 * (int32(r.rows+63) / 64)
	// The column values.
	start = align(start, v.Type.Alignment())
	v.start = r.pointer(start)
	// The end of the offsets for variable width data.
	v.end = r.pointer(r.pageStart(col + 1))
	return v
}