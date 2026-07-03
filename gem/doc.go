// Package gem provides pure value builders for SEMI E30 (GEM) role messages.
//
// Every builder returns a [secs2.SECS2Message] that can be sent via
// Connection.SendSECS2Message on an established HSMS or SECS-I connection.
//
// The transport-agnostic base builder is [secs2.NewMessage]; callers that need
// stream/function pairs not covered here should use it directly.
//
// Equipment-defined identifiers are passed as [secs2.Item] values so callers
// control the SECS-II type (ASCII, integer, binary, …) without coupling to gem.
package gem
