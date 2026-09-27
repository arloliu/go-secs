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

// DialBounder lets a transport bound a dial by the caller of the Open that started this generation.
//
// It is reached by type assertion, the same way GenerationRuntime is:
// the core offers it,
// and an in-module transport's active dial site consults it after wrapping the dial ctx with its own per-attempt timeout (e.g. WithConnectTimeout).
// It is unrelated to GenerationRuntime's per-message generation fencing —
// it fences exactly one thing, the FIRST dial of an Open cycle.
type DialBounder interface {
	// BoundDial returns dialCtx, additionally cancelled
	// when the Open that passed startCtx to Start has its ctx cancelled or is aborted by a concurrent Close.
	//
	// startCtx must be the EXACT ctx value the transport's Start received:
	// the core matches it by identity against the ctx of the Open whose first dial is in flight.
	// For any other startCtx — a reconnect generation's Start, or a call made once that Open has already returned —
	// dialCtx is returned unchanged, with a no-op cancel.
	//
	// cancel must be called once the dial returns, successfully or not;
	// it releases resources bound to this one call and never affects anything beyond it.
	BoundDial(startCtx, dialCtx context.Context) (context.Context, context.CancelFunc)
}

// SocketAdopter lets a transport hand the core a socket together with the identity the transport minted for it.
//
// It is reached by type assertion, the same way GenerationRuntime is,
// and a transport falls back to GenerationRuntime.TCPUpFromGeneration when the runtime does not offer it.
// It is kept apart from GenerationRuntime so that fallback stays reachable:
// a runtime can offer generation fencing without socket identity.
type SocketAdopter interface {
	// AdoptSocketFromGeneration is TCPUpFromGeneration that also records socket as the identity of conn on the generation that adopts it.
	// It reports whether conn was accepted onto a live generation, exactly as TCPUpFromGeneration does;
	// on false the caller still owns conn and must close it.
	AdoptSocketFromGeneration(gen uint64, conn net.Conn, socket uint64) bool
}
