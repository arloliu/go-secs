package tracepack

import "bytes"

// PackInfo is what identifies one pack and describes it: its pack_id, its capture_id and its decoded pack metadata.
//
// A PackInfo is built only from decoded bytes, by Reader.Info or NewPackInfo,
// never from a PackMeta a caller could have changed after it was decoded,
// so it never holds pack metadata the decoder would reject.
// Its accessors return copies.
// The zero PackInfo holds no pack metadata.
type PackInfo struct {
	packID    UUID
	captureID UUID
	// raw holds the pack metadata bytes, which no caller can reach; Meta decodes a fresh copy from them on every call.
	raw []byte
	// meta is decoded from raw and never handed out.
	meta *PackMeta
}

// NewPackInfo returns the PackInfo of a pack from its file header ids and its pack metadata as stored
// (the tracepack format specification §5), as stored beside the pack.
//
// It copies metadata, then decodes the copy as UnmarshalPackMeta does, with the same registry checks and intrinsic rules.
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
	raw := bytes.Clone(metadata)
	meta, err := UnmarshalPackMeta(raw)
	if err != nil {
		return PackInfo{}, err
	}

	return PackInfo{packID: packID, captureID: captureID, raw: raw, meta: meta}, nil
}

// PackID returns the pack's pack_id.
func (p PackInfo) PackID() UUID {
	return p.packID
}

// CaptureID returns the pack's capture_id.
func (p PackInfo) CaptureID() UUID {
	return p.captureID
}

// Meta returns the pack's pack metadata, decoded afresh on every call,
// so a caller may modify the result without affecting p; nil for the zero PackInfo.
func (p PackInfo) Meta() *PackMeta {
	if p.meta == nil {
		return nil
	}
	// The same bytes were decoded without error when p was built, so decoding them again cannot fail.
	meta, _ := UnmarshalPackMeta(p.raw)

	return meta
}
