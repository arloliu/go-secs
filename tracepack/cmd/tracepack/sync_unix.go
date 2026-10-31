//go:build unix

package main

import "os"

// syncDir syncs the directory dir, so that a rename into it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()

		return err
	}

	return d.Close()
}
