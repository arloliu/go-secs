// Package secs1 provides an implementation of the SEMI E4 (SECS-I) block-transfer
// and message protocols running over a TCP/IP stream.
//
// SECS-I (SEMI Equipment Communications Standard, Transport) is the original
// serial communication protocol for semiconductor manufacturing equipment.
// This package adapts the protocol for use over TCP/IP, treating the TCP stream
// as a byte-oriented transport in place of a serial port.
//
// # Protocol Overview
//
// SECS-I is a half-duplex, block-oriented protocol with explicit handshake
// control using single-byte control characters:
//
//   - ENQ (0x05) — Request to Send
//   - EOT (0x04) — Ready to Receive
//   - ACK (0x06) — Correct Reception
//   - NAK (0x15) — Incorrect Reception
//
// Messages are split into blocks of up to 244 data bytes each, with a 10-byte
// header per block. Contention resolution follows the Master/Slave model defined
// in SEMI E4: Equipment is typically Master (wins contention) and Host is Slave
// (yields on contention).
//
// # Timeouts
//
// The protocol defines four timers per SEMI E4 Table 4:
//
//   - T1 (inter-character): maximum gap between bytes during block reception
//   - T2 (protocol): maximum wait for handshake responses (EOT after ENQ, ACK after block)
//   - T3 (reply): maximum wait for a reply message after sending a primary
//   - T4 (inter-block): maximum gap between blocks in a multi-block message
//
// # Relationship to hsms Package
//
// The secs1 package implements the [hsms.Connection] and [hsms.Session] interfaces,
// allowing applications to switch between HSMS-SS and SECS-I transports with minimal
// code changes.
package secs1
