package tracepack

import "bytes"

// PackInfo is what identifies one pack and describes it: its pack_id, its capture_id and its decoded pack metadata.
//
// A PackInfo is built only from decoded bytes, by Reader.Info, NewPackInfo or NewPackInfoMinor,
// never from a PackMeta a caller could have changed after it was decoded,
// so it never holds pack metadata the decoder would reject.
// It keeps the format minor its pack metadata was decoded under, so Meta decodes it the same way every time.
// Its accessors return copies.
// The zero PackInfo holds no pack metadata.
type PackInfo struct {
	packID    UUID
	captureID UUID
	// minor is the format minor raw is decoded under (UnmarshalPackMetaMinor).
	minor uint16
	// raw holds the pack metadata bytes, which no caller can reach; Meta decodes a fresh copy from them on every call.
	raw []byte
	// meta is decoded from raw and never handed out.
	meta *PackMeta
}

// NewPackInfo returns the PackInfo of a pack from its file header ids and its pack metadata as stored
// (the tracepack format specification §5), as stored beside the pack.
//
// It copies metadata, then decodes the copy as UnmarshalPackMeta does, with the same registry checks and intrinsic rules:
// with the newest registry this package knows, as for a pack of an unknown newer minor.
// A caller that knows the pack's format minor calls NewPackInfoMinor.
// A pack_role this package does not know, the retired value included, decodes.
//
// Parameters:
//   - packID: the pack's pack_id.
//   - captureID: the pack's capture_id.
//   - metadata: the pack's pack metadata bytes; the caller may reuse the buffer afterwards.
//
// Returns:
//   - PackInfo: the pack's PackInfo; the zero PackInfo on error.
//   - error: the error of UnmarshalPackMeta.
func NewPackInfo(packID, captureID UUID, metadata []byte) (PackInfo, error) {
	return NewPackInfoMinor(packID, captureID, formatMinorNewest, metadata)
}

// NewPackInfoMinor returns the PackInfo of a pack as NewPackInfo does,
// decoding its pack metadata as UnmarshalPackMetaMinor does for a pack of the format minor its file header states:
// a number a later minor defines keeps the status the pack's minor gives it
// (the tracepack format specification §5 and §14).
//
// Parameters:
//   - packID: the pack's pack_id.
//   - captureID: the pack's capture_id.
//   - minor: the pack's format_minor, from its file header.
//   - metadata: the pack's pack metadata bytes; the caller may reuse the buffer afterwards.
//
// Returns:
//   - PackInfo: the pack's PackInfo; the zero PackInfo on error.
//   - error: the error of UnmarshalPackMetaMinor.
func NewPackInfoMinor(packID, captureID UUID, minor uint16, metadata []byte) (PackInfo, error) {
	raw := bytes.Clone(metadata)
	meta, err := UnmarshalPackMetaMinor(raw, minor)
	if err != nil {
		return PackInfo{}, err
	}

	return PackInfo{packID: packID, captureID: captureID, minor: minor, raw: raw, meta: meta}, nil
}

// PackID returns the pack's pack_id.
func (p PackInfo) PackID() UUID {
	return p.packID
}

// CaptureID returns the pack's capture_id.
func (p PackInfo) CaptureID() UUID {
	return p.captureID
}

// Meta returns the pack's pack metadata, decoded afresh on every call under the format minor p was built with,
// so a caller may modify the result without affecting p; nil for the zero PackInfo.
func (p PackInfo) Meta() *PackMeta {
	if p.meta == nil {
		return nil
	}
	// The same bytes were decoded under the same minor without error when p was built, so decoding them again cannot fail.
	meta, _ := UnmarshalPackMetaMinor(p.raw, p.minor)

	return meta
}
