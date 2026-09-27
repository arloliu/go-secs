package hsms

import (
	"net"
	"strconv"
	"time"
)

// WireDirection says which way a frame crossed the socket (see [WireEvent]).
type WireDirection uint8

const (
	// WireInbound marks a frame read from the peer.
	WireInbound WireDirection = iota + 1
	// WireOutbound marks a frame written to the peer.
	WireOutbound
)

// WireEvent describes one complete HSMS frame as it crossed a socket —
// see [WithWireObserver] for which frames are reported and when.
//
// Frame aliases a buffer the connection owns and is valid only for the duration of the observer call:
// an observer that keeps the bytes copies them, and never writes into Frame.
// Every other field is a plain value and safe to retain.
type WireEvent struct {
	// Direction says whether the frame was read from or written to the peer.
	Direction WireDirection
	// Socket is reserved for the identity of the socket the frame crossed.
	// It is always 0 in this release.
	Socket uint64
	// Generation identifies the connection generation that owns the socket:
	// a per-connection counter that advances each time the connection establishes a new link,
	// so every frame of one link carries the same value and a reconnected link carries a larger one.
	Generation uint64
	// At is taken when the frame was fully read or fully written.
	// It carries a monotonic clock reading,
	// so At.Sub against another event's At measures elapsed time even across a wall-clock step.
	At time.Time
	// Frame holds the wire bytes: the 4-byte length prefix, the 10-byte header, and the message text.
	Frame []byte
}

// wireReporter is the optional transport capability that opts the core's outbound frames into wire observation.
//
// It is a marker reached by type assertion, deliberately not a method of the transport seam,
// so a transport implementation that predates it keeps compiling.
// A transport offers it only when Write puts the handed buffers on its socket byte for byte:
// the HSMS-SS transport does, while the SECS-I transport reports success for an HSMS control frame without any I/O
// and re-frames a data message as SECS-I blocks, so reporting its frames would describe bytes that never crossed its wire.
type wireReporter interface {
	WireReporting()
}

// String returns the string representation of the WireDirection.
func (d WireDirection) String() string {
	switch d {
	case WireInbound:
		return "Inbound"
	case WireOutbound:
		return "Outbound"
	default:
		return "WireDirection(" + strconv.FormatUint(uint64(d), 10) + ")"
	}
}

// ObserveWire hands ev to the observer installed by [WithWireObserver], and returns at once when none is.
//
// It is the capability the HSMS-SS transport reaches by type assertion to report the frames it reads itself;
// it is not part of any interface an application implements or calls.
func (c *connection) ObserveWire(ev WireEvent) {
	if obs := c.cfg.Load().wireObserver; obs != nil {
		obs(ev)
	}
}

// outboundWireObserver returns the wire observer that should see the core's outbound frames,
// or nil when none is installed or the transport does not offer the wire-reporting capability.
// A write site loads it once and uses that one value for both the capture and the report,
// so a concurrent UpdateConfigOptions cannot leave a frame captured but unreported, or reported uncaptured.
func (c *connection) outboundWireObserver() func(WireEvent) {
	obs := c.cfg.Load().wireObserver
	if obs == nil {
		return nil
	}

	if _, ok := c.tr.(wireReporter); !ok {
		return nil
	}

	return obs
}

// captureWireFrame copies the frame held by bufs into this epoch's scratch buffer and returns it.
// The caller holds e.writeMu, which guards the scratch buffer,
// and calls it before handing bufs to the transport, because the write consumes bufs as it goes.
//
// The scratch buffer grows to the largest frame this generation has written with an observer installed,
// and is reused after that,
// so observation adds no allocation per frame once warm;
// it is released with the epoch when the generation ends.
func (e *epoch) captureWireFrame(bufs net.Buffers) []byte {
	n := 0
	for _, b := range bufs {
		n += len(b)
	}

	if cap(e.wireScratch) < n {
		e.wireScratch = make([]byte, n)
	}

	frame := e.wireScratch[:n]
	off := 0
	for _, b := range bufs {
		off += copy(frame[off:], b)
	}

	return frame
}
