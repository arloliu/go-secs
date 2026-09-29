package format

import "hash/crc32"

// CRC returns the CRC-32/ISO-HDLC checksum of b.
//
// This is the IEEE 802.3 / zlib / PNG polynomial
// that the tracepack format specification §2 requires for every checksum in the container:
// 0x04C11DB7 reflected, initial value 0xFFFFFFFF, final XOR 0xFFFFFFFF.
func CRC(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}

// UpdateCRC returns the CRC-32/ISO-HDLC checksum of the bytes crc covers followed by b,
// so a checksum is computed over sequential chunks, starting from 0:
// UpdateCRC(UpdateCRC(0, a), b) equals CRC of a followed by b.
func UpdateCRC(crc uint32, b []byte) uint32 {
	return crc32.Update(crc, crc32.IEEETable, b)
}
