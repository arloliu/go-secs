package redact_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// exampleKeys serves fixed keys; a real caller fetches them from its key custody.
type exampleKeys struct{}

func (exampleKeys) DigestKey(string) ([]byte, error) {
	k := sha256.Sum256([]byte("example digest key"))
	return k[:], nil
}

func (exampleKeys) EncryptionKey(tracepack.UUID, string) ([]byte, error) {
	return bytes.Repeat([]byte{0x42}, redact.KeySize), nil
}

// Example masks the process program body of an S7F3 under a policy with encryption,
// and restores it with the capture's encryption key.
func Example() {
	compiled, err := redact.Compile(redact.Policy{
		ID: "recipes", Version: 1, DigestKeyID: "digest-1",
		Encryption: &redact.Encryption{KeyID: "enc-1"},
		ItemRules:  []redact.ItemRule{{Stream: 7, Function: 3, Path: "2", Domain: "ppbody"}},
	})
	if err != nil {
		panic(err)
	}

	// S7F3 <L[2] <A "PP1"> <B 0x10 0x20 0x30>>, SessionID 1.
	text := []byte{0x01, 0x02, 0x41, 0x03, 'P', 'P', '1', 0x21, 0x03, 0x10, 0x20, 0x30}
	frame := make([]byte, 14, 14+len(text))
	binary.BigEndian.PutUint32(frame, uint32(10+len(text)))
	frame[5], frame[6], frame[7] = 1, 0x87, 3
	frame = append(frame, text...)
	rec := tracepack.Record{Seq: 1, Kind: tracepack.KindData, Payload: frame}

	var capture tracepack.UUID
	masked, entries, err := compiled.Mask(capture, &rec, false, exampleKeys{})
	if err != nil {
		panic(err)
	}
	e := entries[0]
	fmt.Println("path:", *e.ItemPath, "domain:", e.Domain, "ranges:", e.MaskedRanges, "siv bytes:", len(e.SIV))
	fmt.Println("redacted:", masked.Quality.Has(tracepack.QualityRedacted), "header kept:", bytes.Equal(masked.Payload[:23], frame[:23]))

	// A reader reports the record's entries and defects in Item.Redaction, and the policy in the pack header.
	policy := compiled.PackPolicy()
	key, _ := exampleKeys{}.EncryptionKey(capture, *policy.EncryptionKeyID)
	orig, err := redact.Restore(capture, &masked, &tracepack.RecordRedaction{Entries: entries}, &policy, key)
	if err != nil {
		panic(err)
	}
	fmt.Println("restored:", bytes.Equal(orig, frame))
	// Output:
	// path: 2 domain: ppbody ranges: [{23 3}] siv bytes: 16
	// redacted: true header kept: true
	// restored: true
}
