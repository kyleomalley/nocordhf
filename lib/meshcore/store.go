package meshcore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// StoredMessage is one persisted chat entry. Threads live in
// per-key buckets named by the thread ID (e.g. "channel:0",
// "contact:abcdef…"); within each bucket, keys are 8-byte
// big-endian unix-nanos so the default cursor walks chronological.
//
// Same-key writes overwrite — outbound messages are appended as
// Pending, then re-written to the same When-key once the firmware
// confirms delivery. Read paths therefore never see duplicate rows
// for the same logical message.
type StoredMessage struct {
	When     time.Time `json:"when"`
	Text     string    `json:"text"`
	Outgoing bool      `json:"out,omitempty"`
	// Sender is the bare sender name (operator's own callsign for
	// outbound, contact display name or channel-payload prefix for
	// inbound). Stored without any "Name: " envelope so the chat
	// renderer can right-align it in its own column.
	Sender    string  `json:"sender,omitempty"`
	SNR       float64 `json:"snr,omitempty"`
	AckCRC    uint32  `json:"ack,omitempty"`
	Delivered bool    `json:"delivered,omitempty"`
	Failed    bool    `json:"failed,omitempty"`
	// PathLen is the raw Packet.PathLen byte (top 2 bits = hash size,
	// bottom 6 = hash count) captured at receive time. Path is the
	// concatenated path-hash bytes of the route the packet took to
	// reach us. Persisted so the chat row's right-click "Map Trace"
	// works on historical messages — without this the trace relies
	// on the in-memory RxLog ring which rolls past or empties on
	// relaunch. Empty for outbound rows, system rows, and inbound
	// rows where the matching RxLog frame was missed.
	PathLen byte   `json:"plen,omitempty"`
	Path    []byte `json:"path,omitempty"`
}

// Store is the persistent message-history backing for a Client.
// Backed by a single bbolt file (opened with a 2-second flock
// timeout to fail fast if a sibling process already has it).
type Store struct {
	db *bbolt.DB
}

// OpenStore opens (or creates) the message-history database at the
// given path. Pass an absolute path for predictable behaviour under
// macOS bundle launches where cwd is "/" — the GUI uses the same
// working-dir-or-app-support path as nocordhf.log.
func OpenStore(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o644, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("meshcore: open store %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close flushes pending writes and releases the file lock.
// Idempotent.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Append writes msg to the named thread's bucket. Same-key writes
// (msg.When matching an existing entry) overwrite — used to update
// Pending → Delivered / Failed transitions in place.
func (s *Store) Append(thread string, msg StoredMessage) error {
	if s == nil || s.db == nil {
		return errors.New("meshcore: store closed")
	}
	if thread == "" {
		return errors.New("meshcore: empty thread id")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(thread))
		if err != nil {
			return err
		}
		val, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		return b.Put(nanosKey(msg.When), val)
	})
}

// LoadThread returns up to maxRows most-recent messages for one
// thread, in chronological order (oldest first). max <= 0 returns
// every entry. Returns nil when the thread has no bucket — caller
// treats that as "no history yet".
func (s *Store) LoadThread(thread string, max int) ([]StoredMessage, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var out []StoredMessage
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(thread))
		if b == nil {
			return nil
		}
		out = readBucketTail(b, max)
		return nil
	})
	return out, err
}

// LoadAll returns up to maxPerThread most-recent messages for every
// known thread, keyed by thread ID. Used on Client connect to
// restore in-memory chat history before the events goroutine
// starts emitting new rows.
func (s *Store) LoadAll(maxPerThread int) (map[string][]StoredMessage, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	out := map[string][]StoredMessage{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bbolt.Bucket) error {
			rows := readBucketTail(b, maxPerThread)
			if len(rows) > 0 {
				out[string(name)] = rows
			}
			return nil
		})
	})
	return out, err
}

// readBucketTail walks a bucket backward (newest → oldest) up to
// max entries, then returns them in chronological order. Centralised
// so LoadThread and LoadAll share the same JSON-decode + reverse
// path.
func readBucketTail(b *bbolt.Bucket, max int) []StoredMessage {
	c := b.Cursor()
	var rev []StoredMessage
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		var msg StoredMessage
		if err := json.Unmarshal(v, &msg); err != nil {
			continue
		}
		rev = append(rev, msg)
		if max > 0 && len(rev) >= max {
			break
		}
	}
	if len(rev) == 0 {
		return nil
	}
	out := make([]StoredMessage, len(rev))
	for i, m := range rev {
		out[len(rev)-1-i] = m
	}
	return out
}

// nanosKey serialises a wall-clock time to an 8-byte big-endian
// uint64 of unix nanoseconds. Big-endian uint64 sorts byte-wise in
// the same order as the integer, so bbolt's default cursor walks
// chronological without a custom comparator.
func nanosKey(t time.Time) []byte {
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], uint64(t.UnixNano()))
	return k[:]
}

// favoritesBucket is the bbolt bucket name where contact favorites
// are persisted — key = 32-byte raw pubkey, value = empty (presence
// is the boolean). Lives in the same db file as message history so
// a single OpenStore call hydrates everything.
var favoritesBucket = []byte("__favorites")

// PurgeLegacyChannelBuckets deletes channel chat-history buckets
// keyed by the firmware's slot index ("channel:0", "channel:1",
// …). The current keying is "channel:<16-hex-secret-id>" so any
// bucket whose name matches the legacy `channel:<digits>` shape is
// orphaned — it can never be re-attached to a live channel because
// the lookup path no longer produces that key shape. Returns the
// number of buckets deleted; safe to call repeatedly (no-op once
// the legacy buckets are gone).
//
// Worth doing because legacy buckets are silently displayed by
// Store.LoadAll, which seeds in-memory chat history on connect —
// without the purge, an operator who reuses a slot would see the
// previous occupant's messages bleed in until the bucket grew
// past whatever max-rows window the GUI applies.
func (s *Store) PurgeLegacyChannelBuckets() (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	deleted := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		var stale [][]byte
		err := tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			if isLegacyChannelBucketName(name) {
				stale = append(stale, append([]byte(nil), name...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, name := range stale {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
			deleted++
		}
		return nil
	})
	return deleted, err
}

// isLegacyChannelBucketName matches "channel:<one-or-more-digits>"
// — the pre-secret-keying thread ID format. The new format is
// "channel:<16 hex chars>", so the digit-only suffix can't collide
// with a current key (16 hex chars is always longer than 3 digits
// for any plausible slot index, and contains a-f for any non-zero
// channel given a SHA-256 prefix).
func isLegacyChannelBucketName(name []byte) bool {
	const prefix = "channel:"
	if len(name) <= len(prefix) {
		return false
	}
	if string(name[:len(prefix)]) != prefix {
		return false
	}
	suffix := name[len(prefix):]
	for _, b := range suffix {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// SetFavorite marks (on=true) or unmarks (on=false) a contact as
// favourite. Persisted across launches so the operator's pinned
// peers survive a relaunch + bbolt re-open.
func (s *Store) SetFavorite(pub PubKey, on bool) error {
	if s == nil || s.db == nil {
		return errors.New("meshcore: store closed")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(favoritesBucket)
		if err != nil {
			return err
		}
		if on {
			return b.Put(pub[:], nil)
		}
		return b.Delete(pub[:])
	})
}

// pendingAdvertsBucket holds adverts seen while the radio's
// auto-add-contacts mode was off — the firmware delivered them as
// PushNewAdvert (rich Contact-shaped record) but never persisted
// them. We hang on to them locally so the operator can see them
// on the map without admitting them to the radio's contacts table.
// Keyed by 32-byte pubkey, value = JSON-encoded StoredPendingAdvert.
var pendingAdvertsBucket = []byte("__pending_adverts")

// StoredPendingAdvert is the serialised form of a pending advert.
// FirstSeen lets the GUI surface "discovered N hours ago"; LastSeen
// gets bumped every time the same pubkey re-advertises.
type StoredPendingAdvert struct {
	PubKey     PubKey    `json:"pk"`
	Type       AdvType   `json:"type"`
	Flags      byte      `json:"flags,omitempty"`
	OutPathLen int8      `json:"plen,omitempty"`
	OutPath    []byte    `json:"path,omitempty"` // first OutPathLen bytes only
	AdvName    string    `json:"name"`
	AdvLatE6   int32     `json:"lat,omitempty"`
	AdvLonE6   int32     `json:"lon,omitempty"`
	LastAdvert time.Time `json:"last_adv"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
}

// AsContact converts back to the Contact shape AddUpdateContact
// expects when promoting to a real contact.
func (p StoredPendingAdvert) AsContact() Contact {
	var c Contact
	c.PubKey = p.PubKey
	c.Type = p.Type
	c.Flags = p.Flags
	c.OutPathLen = p.OutPathLen
	copy(c.OutPath[:], p.OutPath)
	c.AdvName = p.AdvName
	c.AdvLatE6 = p.AdvLatE6
	c.AdvLonE6 = p.AdvLonE6
	c.LastAdvert = p.LastAdvert
	return c
}

// SavePendingAdvert upserts a pending-advert record. If the pubkey
// is already present, FirstSeen is preserved and LastSeen is
// refreshed; the rest of the fields are overwritten with the new
// values from this advert (name / lat / lon may have changed since
// last time we saw this node).
func (s *Store) SavePendingAdvert(p StoredPendingAdvert) error {
	if s == nil || s.db == nil {
		return errors.New("meshcore: store closed")
	}
	now := time.Now().UTC()
	if p.LastSeen.IsZero() {
		p.LastSeen = now
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(pendingAdvertsBucket)
		if err != nil {
			return err
		}
		// Preserve FirstSeen across re-advertisements.
		if existing := b.Get(p.PubKey[:]); existing != nil {
			var prev StoredPendingAdvert
			if err := json.Unmarshal(existing, &prev); err == nil && !prev.FirstSeen.IsZero() {
				p.FirstSeen = prev.FirstSeen
			}
		}
		if p.FirstSeen.IsZero() {
			p.FirstSeen = now
		}
		val, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return b.Put(p.PubKey[:], val)
	})
}

// LoadPendingAdverts returns every pending advert in the store,
// keyed by pubkey. Used on connect to seed the in-memory map so
// adverts persist across launches even though the radio doesn't
// know about them.
func (s *Store) LoadPendingAdverts() (map[PubKey]StoredPendingAdvert, error) {
	out := map[PubKey]StoredPendingAdvert{}
	if s == nil || s.db == nil {
		return out, nil
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pendingAdvertsBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) != 32 {
				return nil
			}
			var p StoredPendingAdvert
			if err := json.Unmarshal(v, &p); err != nil {
				return nil // skip malformed entries rather than fail the whole load
			}
			var pk PubKey
			copy(pk[:], k)
			out[pk] = p
			return nil
		})
	})
	return out, err
}

// blockedAdvertsBucket persists pubkeys the operator has chosen to
// permanently silence. Without this, "Discard" on a pending advert
// only drops the record until the next periodic re-advertisement
// (which on a busy mesh is constant), so spammy nodes keep
// reappearing on the map. Block makes the silence durable.
var blockedAdvertsBucket = []byte("__blocked_adverts")

// BlockAdvert marks a pubkey as permanently blocked. Future
// PushNewAdvert events for this pubkey are dropped at the GUI
// layer before they reach the pending-advert store. Idempotent.
func (s *Store) BlockAdvert(pub PubKey) error {
	if s == nil || s.db == nil {
		return errors.New("meshcore: store closed")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(blockedAdvertsBucket)
		if err != nil {
			return err
		}
		return b.Put(pub[:], nil)
	})
}

// UnblockAdvert removes a pubkey from the block list. The next
// advert from this node will be admitted into the pending store
// as usual.
func (s *Store) UnblockAdvert(pub PubKey) error {
	if s == nil || s.db == nil {
		return errors.New("meshcore: store closed")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(blockedAdvertsBucket)
		if b == nil {
			return nil
		}
		return b.Delete(pub[:])
	})
}

// LoadBlockedAdverts returns the set of pubkeys the operator has
// blocked. Hydrated on connect so the in-memory filter survives
// relaunch.
func (s *Store) LoadBlockedAdverts() (map[PubKey]bool, error) {
	out := map[PubKey]bool{}
	if s == nil || s.db == nil {
		return out, nil
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(blockedAdvertsBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			if len(k) != 32 {
				return nil
			}
			var pk PubKey
			copy(pk[:], k)
			out[pk] = true
			return nil
		})
	})
	return out, err
}

// DeletePendingAdvert removes a pending-advert record. Called after
// a successful AddUpdateContact promotion so the same node doesn't
// show up in both the contacts list and the pending overlay.
func (s *Store) DeletePendingAdvert(pub PubKey) error {
	if s == nil || s.db == nil {
		return errors.New("meshcore: store closed")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pendingAdvertsBucket)
		if b == nil {
			return nil
		}
		return b.Delete(pub[:])
	})
}

// LoadFavorites returns the set of pubkeys currently marked as
// favourite. Used on connect to seed the in-memory mcFavorites map.
func (s *Store) LoadFavorites() (map[PubKey]bool, error) {
	out := map[PubKey]bool{}
	if s == nil || s.db == nil {
		return out, nil
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(favoritesBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			if len(k) != 32 {
				return nil
			}
			var pk PubKey
			copy(pk[:], k)
			out[pk] = true
			return nil
		})
	})
	return out, err
}
