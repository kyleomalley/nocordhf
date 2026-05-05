package meshcore

import "crypto/sha256"

// DeriveHashtagChannelSecret returns the 16-byte AES-128 key for a
// "#"-prefixed community channel. The convention used by the iOS /
// Flutter MeshCore clients (and now this one) is:
//
//	secret = SHA-256(channel name) truncated to the first 16 bytes
//
// — and the channel name INCLUDES the leading `#`. This lets
// operators "auto-join" community channels (#volcano, #meshbud, etc.)
// by typing the name; the channel name itself is the shared secret
// material, distributed naturally as people refer to the channel.
//
// Non-hashtag channels (e.g. firmware-default "Public", or any
// privately-named slot) still require an explicitly-shared 16-byte
// secret since there's no name-based contract for those.
func DeriveHashtagChannelSecret(name string) [16]byte {
	sum := sha256.Sum256([]byte(name))
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

// IsHashtagChannelName returns true when the given name follows the
// hashtag-channel convention (starts with `#`). Used by callers to
// decide whether to offer auto-derivation in their Add Channel UX.
func IsHashtagChannelName(name string) bool {
	return len(name) > 1 && name[0] == '#'
}

// ChannelHash returns the 1-byte channel-hash identifier the
// firmware uses on the wire to tag inbound channel messages —
// SHA-256(secret)[0]. Mirrors the upstream meshcore.js
// calcChannelHash so cross-client comparisons line up. The wire
// "channel index" byte in RespChannelMsgRecv / RespChannelMsgRecvV3
// is this hash, NOT the firmware's channel slot index, so callers
// matching incoming channel messages to a configured channel must
// compare against this value.
func ChannelHash(secret [16]byte) byte {
	sum := sha256.Sum256(secret[:])
	return sum[0]
}
