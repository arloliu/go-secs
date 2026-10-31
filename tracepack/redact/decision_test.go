package redact_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// maskDecision is what a consumer does with an error of Compile, Mask or Digest, the first match deciding.
type maskDecision string

const (
	// decideDefer: the record cannot be screened as it is; defer it, never write it unscreened.
	decideDefer maskDecision = "defer the record"
	// decideFixConfig: a key, policy or domain that cannot be used; fix the configuration.
	decideFixConfig maskDecision = "fix the configuration"
	// decideStop: ctx is done; stop.
	decideStop maskDecision = "stop"
	// decideMaskTransient: an error of the caller's Keys; it may be transient.
	decideMaskTransient maskDecision = "transient"
)

// restoreDecision is what the holder of a key does with an error of Restore, the first match deciding.
type restoreDecision string

const (
	// decideNotRestorable: the record cannot be restored under this policy, or holds no original.
	decideNotRestorable restoreDecision = "not restorable"
	// decideWrongKeyMaterial: the key is not an encryption key of this algorithm.
	decideWrongKeyMaterial restoreDecision = "wrong key material"
	// decideNotAuthentic: a wrong key, or masked bytes or a siv that changed.
	decideNotAuthentic restoreDecision = "not authentic"
	// decideDefective: the stored entries are defective.
	decideDefective restoreDecision = "defective entries"
	// decideRestoreOther: an error no row names; no call of the tests reaches it.
	decideRestoreOther restoreDecision = "other"
)

// decideMask returns the decision for an error of Compile, Mask or Digest, in the documented order:
// this package's sentinels first, before any cause they wrap.
func decideMask(err error) maskDecision {
	switch {
	case errors.Is(err, redact.ErrCannotScreen), errors.Is(err, redact.ErrRedacted), errors.Is(err, redact.ErrNotUnobserved):
		return decideDefer
	case errors.Is(err, redact.ErrKeySize), errors.Is(err, redact.ErrInvalidPolicy), errors.Is(err, redact.ErrDomain):
		return decideFixConfig
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return decideStop
	default:
		return decideMaskTransient
	}
}

// decideRestore returns the decision for an error of Restore, in the documented order.
func decideRestore(err error) restoreDecision {
	switch {
	case errors.Is(err, redact.ErrNoEncryption), errors.Is(err, redact.ErrUnsupportedAlgorithm), errors.Is(err, redact.ErrNotRestorable):
		return decideNotRestorable
	case errors.Is(err, redact.ErrKeySize):
		return decideWrongKeyMaterial
	case errors.Is(err, redact.ErrAuthentication):
		return decideNotAuthentic
	case errors.Is(err, tracepack.ErrRedactionDefect):
		return decideDefective
	default:
		return decideRestoreOther
	}
}

// TestMaskDecision drives each row of the decision table of Compile, Mask and Digest,
// including ErrCannotScreen over a decoder's cause, which the redact sentinel decides while the cause stays matchable.
func TestMaskDecision(t *testing.T) {
	t.Parallel()

	c := mustCompile(t, baselinePolicy(true))
	sensitive := frame(7, 3, ppbodyText("PP1", []byte("SECRET-RECIPE-01")))
	mask := func(ctx context.Context, r *tracepack.Record, unobserved bool, keys redact.Keys) error {
		_, _, err := c.Mask(ctx, captureA, r, unobserved, keys)
		return err
	}
	errUnavailable := errors.New("key service unavailable")

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want maskDecision
		// also holds the causes the error also matches, which the first row overrides.
		also []error
	}{
		{name: "Mask: text nested deeper than the walker's limit", want: decideDefer, also: []error{redact.ErrCannotScreen}, err: func(t *testing.T) error {
			b := item(0o10, 1, 2, 3)
			for range 64 {
				b = list(b)
			}

			return mask(t.Context(), dataRecord(1, frame(7, 3, list(b, item(0o10, 9)))), false, &testKeys{})
		}},
		{name: "Mask: an annotation that does not decode", want: decideDefer, also: []error{redact.ErrCannotScreen, tracepack.ErrCorrupt}, err: func(t *testing.T) error {
			return mask(t.Context(), &tracepack.Record{Seq: 2, Kind: tracepack.KindAnnotation, Payload: []byte{0, 1, 2}}, false, &testKeys{})
		}},
		{name: "Mask: a record already redacted", want: decideDefer, also: []error{redact.ErrRedacted}, err: func(t *testing.T) error {
			r := dataRecord(3, sensitive)
			r.Quality |= tracepack.QualityRedacted

			return mask(t.Context(), r, false, &testKeys{})
		}},
		{name: "Mask: a record called unobserved without bit 7", want: decideDefer, also: []error{redact.ErrNotUnobserved}, err: func(t *testing.T) error {
			return mask(t.Context(), dataRecord(5, frame(7, 3, nil)), true, &testKeys{})
		}},
		{name: "Mask: an encryption key of 32 bytes", want: decideFixConfig, also: []error{redact.ErrKeySize}, err: func(t *testing.T) error {
			return mask(t.Context(), dataRecord(4, sensitive), false, &failingKeys{encKey: make([]byte, 32)})
		}},
		{name: "Compile: an empty domain", want: decideFixConfig, also: []error{redact.ErrInvalidPolicy, redact.ErrDomain}, err: func(*testing.T) error {
			_, err := redact.Compile(redact.Policy{ItemRules: []redact.ItemRule{{Stream: 7, Function: 3, Path: "2"}}})
			return err
		}},
		{name: "Compile: an ID that is not valid UTF-8", want: decideFixConfig, also: []error{redact.ErrInvalidPolicy}, err: func(*testing.T) error {
			p := baselinePolicy(false)
			p.ID = "\xff"
			_, err := redact.Compile(p)

			return err
		}},
		{name: "Digest: an empty domain", want: decideFixConfig, also: []error{redact.ErrDomain}, err: func(*testing.T) error {
			_, err := redact.Digest(make([]byte, 32), "", nil)
			return err
		}},
		{name: "Mask: ctx cancelled", want: decideStop, err: func(t *testing.T) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			return mask(ctx, dataRecord(4, sensitive), false, &testKeys{})
		}},
		{name: "Mask: the digest key unavailable", want: decideMaskTransient, also: []error{errUnavailable}, err: func(t *testing.T) error {
			return mask(t.Context(), dataRecord(4, sensitive), false, &failingKeys{digestErr: errUnavailable})
		}},
		{name: "Mask: the encryption key unavailable", want: decideMaskTransient, also: []error{errUnavailable}, err: func(t *testing.T) error {
			return mask(t.Context(), dataRecord(4, sensitive), false, &failingKeys{encErr: errUnavailable})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.err(t)
			require.Error(t, err)
			assert.Equal(t, tt.want, decideMask(err), "%v", err)
			for _, c := range tt.also {
				assert.ErrorIs(t, err, c)
			}
		})
	}
}

// TestRestoreDecision drives each row of Restore's decision table.
func TestRestoreDecision(t *testing.T) {
	t.Parallel()

	ppbody := frame(7, 3, ppbodyText("PP1", []byte("SECRET-RECIPE-01")))
	tests := []struct {
		name string
		edit func(in *restoreInput)
		want restoreDecision
		also error
	}{
		{"a policy without encryption", func(in *restoreInput) { in.policy.EncryptionAlgorithm = nil }, decideNotRestorable, redact.ErrNoEncryption},
		{"an unsupported algorithm", func(in *restoreInput) {
			alg := tracepack.EncryptionAlgorithm(200)
			in.policy.EncryptionAlgorithm = &alg
		}, decideNotRestorable, redact.ErrUnsupportedAlgorithm},
		{"no Marking", func(in *restoreInput) { in.rr = nil }, decideNotRestorable, redact.ErrNotRestorable},
		{"a key of 32 bytes", func(in *restoreInput) { in.key = in.key[:32] }, decideWrongKeyMaterial, redact.ErrKeySize},
		{"another capture's key", func(in *restoreInput) { in.key = encryptionKey(captureB, encKeyID) }, decideNotAuthentic, redact.ErrAuthentication},
		{"a flipped siv", func(in *restoreInput) { in.rr.Entries[0].SIV[0] ^= 1 }, decideNotAuthentic, redact.ErrAuthentication},
		{"a Marking listing a defect", func(in *restoreInput) {
			in.rr.Defects = []tracepack.RedactionDefect{{Seq: in.r.Seq, Entry: 0, Err: errors.New("defect")}}
		}, decideDefective, tracepack.ErrRedactionDefect},
		{"a wholly masked Marking", func(in *restoreInput) { in.rr.WhollyMasked = true }, decideDefective, tracepack.ErrRedactionDefect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := maskedInput(t, 40, ppbody)
			tt.edit(&in)
			got, err := in.restore(t)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Equal(t, tt.want, decideRestore(err), "%v", err)
			assert.ErrorIs(t, err, tt.also)
		})
	}
}
