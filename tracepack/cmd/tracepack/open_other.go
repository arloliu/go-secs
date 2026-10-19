//go:build !unix

package main

// openFlags adds nothing to the open of a pack where the system has no non-blocking open.
const openFlags = 0
