# SECS-I Transaction & Transport

This document covers the logical transaction layer and the transport specifics for SECS-I over TCP/IP.

## 1. Transactions

A **Transaction** consists of a **Primary Message** and an optional **Secondary (Reply) Message**.

### Identification
*   **Primary Message**: `Function` is Odd (e.g., S1F1). `W-Bit` = 1 usually.
*   **Secondary Message**: `Function` is Even (e.g., S1F2). `Function` = `Primary Function + 1`.

### System Bytes Matching
The **System Bytes** (Bytes 6-9 in Header) are the unique identifier for a transaction.
*   **Request**: Sender generates a unique System Byte sequence.
*   **Reply**: Receiver **MUST** echo the exact same System Bytes in the Reply Message header.

### Usage
1.  **Host** sends S1F1 (System Bytes: `0x00000001`).
2.  **Equipment** receives S1F1.
3.  **Equipment** sends S1F2. Header must contain System Bytes `0x00000001`.
4.  **Host** matches S1F2 to S1F1 using System Bytes and marks transaction as complete.

## 2. Multi-Block Messages

Messages larger than 244 bytes (Body size) must be split into multiple blocks.

### Mechanism
*   **E-Bit**:
    *   `0`: More blocks to follow.
    *   `1`: Last block of the message.
*   **Block Number**:
    *   Starts at 1.
    *   Increments by 1 for each subsequent block.
    *   Receiver must verify the sequence ($B_n = B_{n-1} + 1$).

### Receiving Process
1.  Receive Block with `E-Bit = 0`. Notifies start of multi-block.
2.  Start **T4 Timer** (Inter-Block Timeout).
3.  Buffer the block data.
4.  Wait for next block.
    *   If correct Block Number and System Bytes: Reset T4, Buffer data.
    *   If T4 expires: Discard entire message.
5.  Receive Block with `E-Bit = 1`.
    *   Assemble all buffered bodies + last block body.
    *   Pass full message to application.

## 3. SECS-I on TCP/IP (Transport)

While SECS-I is originally a serial protocol (RS-232), this implementation runs over TCP/IP.

### Stream vs Packet
*   **Serial**: Bytes arrive one by one. T1 is crucial.
*   **TCP**: Stream of bytes. Fragmentation may occur.
    *   A single `read()` might return part of a block, or multiple blocks.
    *   **Implementation Note**: The reader must buffer incoming bytes and parse them according to the Length Byte to separate valid SECS-I blocks.

### Connection Modes
*   **Passive (Server)**: Binds to a port and listens. (Usually Equipment).
*   **Active (Client)**: Connects to a target IP:Port. (Usually Host).

The logical SECS-I protocol (Handshake, T1-T4) runs **on top** of this TCP stream, treating the TCP connection as the raw serial wire.
