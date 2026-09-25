// Package gencap declares the generation-aware back-channel
// that hsms's connection core offers and hsmsss's transport consumes.
//
// The capability is reached by type assertion, deliberately NOT through hsms.TransportRuntime:
// widening that exported interface would break external implementers.
// Declaring it once here, rather than once in each package,
// makes the compiler catch drift between the offering and the consuming side.
package gencap

import (
	"context"
	"net"
)

// GenerationRuntime is the complete set of generation-aware back-channel methods.
// Each report names the generation it belongs to,
// so a report arriving out of a generation that has already ended is discarded rather than applied to its successor.
//
// M and C stand for hsms.Message and hsms.TransitionCause.
// They are type parameters only because this package cannot import hsms without an import cycle;
// both hsms and hsmsss instantiate GenerationRuntime[hsms.Message, hsms.TransitionCause],
// so the two sides name one identical interface type.
type GenerationRuntime[M any, C any] interface {
	CurrentGeneration() uint64
	TCPUpFromGeneration(gen uint64, conn net.Conn) bool
	TCPDownFromGeneration(gen uint64, cause error, transitionCause C)
	CommitSelectedFromGeneration(gen uint64) bool
	SelectLostFromGeneration(gen uint64) bool
	T7ExpiredFromGeneration(gen uint64)
	SendAsyncFromGeneration(ctx context.Context, gen uint64, msg M) error
	WriteMessageFromGeneration(ctx context.Context, gen uint64, msg M) (M, error)

	// DeliverOwnedFrameFromGeneration and RouteReplyFromGeneration fence the INBOUND path the same way the outbound methods above fence sends:
	// each admits or routes on behalf of gen, the generation whose recv goroutine actually read the bytes,
	// rather than whatever generation is current when the call runs.
	DeliverOwnedFrameFromGeneration(gen uint64, frame []byte) error
	RouteReplyFromGeneration(gen uint64, msg M) bool
}
