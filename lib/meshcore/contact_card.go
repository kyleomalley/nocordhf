package meshcore

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// ContactCard is the in-channel-shareable representation of a
// contact. It is intentionally MUCH smaller than the full signed
// advert packet — small enough to fit a base64-encoded form into
// a single MeshCore channel text message (140-byte on-air limit).
// A signed-advert share would need multi-message chunking which
// is brittle on a lossy mesh; a card share trades the embedded
// signature for one-shot delivery and lets the receiving radio
// re-verify identity automatically when the next on-air advert
// from the pubkey arrives (the firmware tracks per-pubkey
// latest-timestamp to reject impersonation attempts).
//
// Trust model: receivers should treat a card the way they treat
// any other channel chat — they trust whoever had the channel
// secret enough to post in the channel at all. For broader
// public channels the card is "discovery", not "verification".
//
// Wire format (raw bytes):
//
//	[1B version=0x01]
//	[32B pubkey]
//	[1B type (AdvType)]
//	[4B advLatE6  int32 LE]
//	[4B advLonE6  int32 LE]
//	[1B name length N (max 32)]
//	[N B  UTF-8 name]
//
// Total: 43 + N bytes (max 75) → ~60-100 base64 chars.
type ContactCard struct {
	PubKey   PubKey
	Type     AdvType
	AdvLatE6 int32
	AdvLonE6 int32
	Name     string
}

const (
	contactCardVersion = 0x01
	// ContactCardURLPrefix is the in-chat marker that opens a card.
	// Lowercase fixed scheme so URL detection is case-insensitive
	// in the parser. The base64 form is unpadded URL-safe so it
	// doesn't include `=` (which would mis-render as a query
	// terminator on some clients) or `+` / `/` (which break URL
	// matching heuristics).
	ContactCardURLPrefix = "mc://contact/"
	contactCardMaxName   = 32
)

var (
	// ErrCardWrongVersion is returned when DecodeContactCard sees
	// a version byte it doesn't recognise. Forwards-compatibility:
	// a future version bump can carry richer fields without
	// needing to hijack the URL scheme.
	ErrCardWrongVersion = errors.New("meshcore: contact card has unsupported version")
	// ErrCardTooShort fires when the payload bytes are shorter
	// than the minimum frame header.
	ErrCardTooShort = errors.New("meshcore: contact card truncated")
	// ErrCardNameTooLong fires when the name exceeds the
	// firmware's 32-byte cap on advert name length.
	ErrCardNameTooLong = errors.New("meshcore: contact name exceeds 32 bytes")
)

// EncodeContactCard serialises c to the wire format and wraps the
// result in the mc://contact/<base64> URL form ready to embed in
// a channel chat message. Truncates names that would otherwise
// exceed the firmware's 32-byte cap rather than failing — the
// caller usually wants a best-effort share, not an error.
func EncodeContactCard(c ContactCard) string {
	name := c.Name
	if len(name) > contactCardMaxName {
		name = name[:contactCardMaxName]
	}
	buf := make([]byte, 0, 43+len(name))
	buf = append(buf, contactCardVersion)
	buf = append(buf, c.PubKey[:]...)
	buf = append(buf, byte(c.Type))
	buf = appendInt32LE(buf, c.AdvLatE6)
	buf = appendInt32LE(buf, c.AdvLonE6)
	buf = append(buf, byte(len(name)))
	buf = append(buf, name...)
	return ContactCardURLPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// DecodeContactCard parses an mc://contact/<base64> URL back into
// a ContactCard. Returns an error for malformed input — caller
// renders the segment as plain text in that case so a typo'd
// share doesn't appear as an empty pill.
func DecodeContactCard(url string) (ContactCard, error) {
	var c ContactCard
	if !strings.HasPrefix(url, ContactCardURLPrefix) {
		return c, fmt.Errorf("meshcore: not a contact-card URL")
	}
	raw, err := base64.RawURLEncoding.DecodeString(url[len(ContactCardURLPrefix):])
	if err != nil {
		return c, fmt.Errorf("meshcore: contact card base64: %w", err)
	}
	if len(raw) < 43 {
		return c, ErrCardTooShort
	}
	if raw[0] != contactCardVersion {
		return c, ErrCardWrongVersion
	}
	copy(c.PubKey[:], raw[1:33])
	c.Type = AdvType(raw[33])
	c.AdvLatE6 = int32(binary.LittleEndian.Uint32(raw[34:38]))
	c.AdvLonE6 = int32(binary.LittleEndian.Uint32(raw[38:42]))
	nameLen := int(raw[42])
	if nameLen > contactCardMaxName {
		return c, ErrCardNameTooLong
	}
	if 43+nameLen > len(raw) {
		return c, ErrCardTooShort
	}
	c.Name = string(raw[43 : 43+nameLen])
	return c, nil
}

// AsContact converts a card into the Contact shape AddUpdateContact
// expects when the recipient promotes the share into their radio's
// contacts table. OutPath is left empty — the firmware will
// populate it from the next inbound packet that takes a path
// through this contact.
func (c ContactCard) AsContact() Contact {
	var out Contact
	out.PubKey = c.PubKey
	out.Type = c.Type
	out.AdvName = c.Name
	out.AdvLatE6 = c.AdvLatE6
	out.AdvLonE6 = c.AdvLonE6
	return out
}
