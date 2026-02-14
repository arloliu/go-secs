# SECS-I Block Structure

This document describes the structure of a SECS-I block as used in the `go-secs` implementation.

## 1. Block Layout

A SECS-I block consists of a sequence of bytes with a total length between 13 and 257 bytes.

| Field | Size (Bytes) | Description |
| :--- | :--- | :--- |
| **Length Byte** | 1 | The number of bytes in the Header + Body. Does not include itself or the Checksum. Value range: 10 to 254. |
| **Header** | 10 | Control information including Device ID, Stream/Function, and Block Number. |
| **Body** | 0 - 244 | The SECS-II message data. |
| **Checksum** | 2 | 16-bit unsigned integer checksum of the Header and Body. |

**Total Length Formula:**
`Total Length = 1 (Length Byte) + Length (Header + Body) + 2 (Checksum)`

## 2. Header Structure

The 10-byte header is structured as follows:

| Byte Offset | Bit 7 (MSB) | Bit 0-6 | Description |
| :--- | :--- | :--- | :--- |
| **0** | **R-Bit** | Device ID (Upper 7 bits) | **R-Bit**: 0 = To Equipment, 1 = To Host. <br> **Device ID**: 15-bit integer identifying the equipment. |
| **1** | Device ID (Lower 8 bits) | | |
| **2** | **W-Bit** | Stream (Upper 7 bits) | **W-Bit**: 1 = Reply Expected, 0 = No Reply. <br> **Stream**: SECS-II Stream number (0-127). |
| **3** | Function (8 bits) | | **Function**: SECS-II Function number (0-255). |
| **4** | **E-Bit** | Block No (Upper 7 bits) | **E-Bit**: 1 = Last Block of Message, 0 = More blocks follow. <br> **Block No**: 15-bit integer. 1-32767. Reset to 1 for new message. |
| **5** | Block No (Lower 8 bits) | | |
| **6** | System Byte 1 | | **System Bytes**: 4-byte unique ID for transaction matching. |
| **7** | System Byte 2 | | Must be unique for open transactions. |
| **8** | System Byte 3 | | Replies must echo the Primary Message's System Bytes. |
| **9** | System Byte 4 | | |

## 3. Checksum Calculation

The checksum is a 16-bit unsigned integer calculated as the arithmetic sum of the unsigned values of all bytes in the **Header** and **Body**.

**Algorithm:**
```go
func CalculateChecksum(header []byte, body []byte) uint16 {
    var sum uint32
    for _, b := range header {
        sum += uint32(b)
    }
    for _, b := range body {
        sum += uint32(b)
    }
    return uint16(sum & 0xFFFF)
}
```

*Note: The Length Byte is NOT included in the checksum calculation.*
