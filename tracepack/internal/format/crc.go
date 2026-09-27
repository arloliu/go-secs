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
