package meshcore

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStoreRoundTrip confirms Append + LoadThread preserves rows in
// chronological order and that same-key writes overwrite (the
// Pending → Delivered update path).
func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	want := []StoredMessage{
		{When: base.Add(0 * time.Second), Text: "first"},
		{When: base.Add(1 * time.Second), Text: "second", Outgoing: true, AckCRC: 0xCAFE, Delivered: false},
		{When: base.Add(2 * time.Second), Text: "third"},
	}
	for _, m := range want {
		if err := s.Append("contact:abc", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Update the second message to Delivered (same When key).
	updated := want[1]
	updated.Delivered = true
	if err := s.Append("contact:abc", updated); err != nil {
		t.Fatalf("append update: %v", err)
	}

	got, err := s.LoadThread("contact:abc", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len got=%d want=3", len(got))
	}
	if got[0].Text != "first" || got[1].Text != "second" || got[2].Text != "third" {
		t.Errorf("order wrong: %+v", got)
	}
	if !got[1].Delivered {
		t.Errorf("update lost: %+v", got[1])
	}
}

// TestStoreMaxPerThread caps the returned slice to the most-recent
// entries (used to cap chat history at maxRows on app launch).
func TestStoreMaxPerThread(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		_ = s.Append("channel:0", StoredMessage{
			When: base.Add(time.Duration(i) * time.Second),
			Text: "msg",
		})
	}
	got, err := s.LoadThread("channel:0", 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len got=%d want=3", len(got))
	}
	// Last 3 should be the newest 3 in chronological order.
	if !got[0].When.Equal(base.Add(7*time.Second)) ||
		!got[2].When.Equal(base.Add(9*time.Second)) {
		t.Errorf("tail window wrong: %+v", got)
	}
}

// TestStoreLoadAll returns every thread's tail.
func TestStoreLoadAll(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().UTC()
	_ = s.Append("contact:abc", StoredMessage{When: now, Text: "hi a"})
	_ = s.Append("channel:0", StoredMessage{When: now, Text: "hi 0"})

	all, err := s.LoadAll(10)
	if err != nil {
		t.Fatalf("loadall: %v", err)
	}
	if len(all["contact:abc"]) != 1 || len(all["channel:0"]) != 1 {
		t.Fatalf("missing thread entries: %+v", all)
	}
}
