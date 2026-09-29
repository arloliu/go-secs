package format

import "slices"

// TransposeHeaders appends to dst the header section of count record headers of headerLen bytes each,
// given as consecutive rows in rows, and returns the extended slice.
// The header section stores the headers column by column (the tracepack format specification §6):
// byte j of record i's header is at offset j × count + i of the section.
//
// rows holds exactly count × headerLen bytes; the caller checked that product with HeaderSectionLen.
// A block of one record is laid out as its header's bytes in order.
func TransposeHeaders(dst, rows []byte, count, headerLen int) []byte {
	if count <= 1 {
		return append(dst, rows...)
	}

	start := len(dst)
	dst = slices.Grow(dst, len(rows))[:start+len(rows)]
	section := dst[start:]
	for i := range count {
		row := rows[i*headerLen : (i+1)*headerLen]
		for j, b := range row {
			section[j*count+i] = b
		}
	}

	return dst
}

// UntransposeHeaders appends to dst the count record headers of headerLen bytes each gathered from section,
// a header section stored column by column (the tracepack format specification §6),
// as consecutive rows, and returns the extended slice: TransposeHeaders reversed.
//
// section holds exactly count × headerLen bytes; the caller checked that product with HeaderSectionLen.
func UntransposeHeaders(dst, section []byte, count, headerLen int) []byte {
	if count <= 1 {
		return append(dst, section...)
	}

	start := len(dst)
	dst = slices.Grow(dst, len(section))[:start+len(section)]
	rows := dst[start:]
	for i := range count {
		row := rows[i*headerLen : (i+1)*headerLen]
		for j := range row {
			row[j] = section[j*count+i]
		}
	}

	return dst
}
