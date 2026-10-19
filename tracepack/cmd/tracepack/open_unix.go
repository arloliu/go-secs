//go:build unix

package main

import "syscall"

// openFlags opens a pack without blocking, so that a FIFO is refused instead of waiting for a writer.
const openFlags = syscall.O_NONBLOCK
