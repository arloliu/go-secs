//go:build !unix

package main

// syncDir does nothing where the system offers no sync of a directory.
func syncDir(string) error { return nil }
