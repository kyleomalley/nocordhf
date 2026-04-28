package cat

import "sync"

// AtomicRadio is a thread-safe Radio wrapper whose underlying implementation
// can be hot-swapped via Swap. All Radio methods are safe to call concurrently
// and when the inner radio is nil — they return zero values or errors.
type AtomicRadio struct {
	mu sync.RWMutex
	r  Radio
}

// NewAtomicRadio returns an AtomicRadio wrapping r (which may be nil).
func NewAtomicRadio(r Radio) *AtomicRadio {
	return &AtomicRadio{r: r}
}

// Swap replaces the underlying radio with r (which may be nil), closing the
// previous one. Safe to call concurrently.
func (a *AtomicRadio) Swap(r Radio) {
	a.mu.Lock()
	old := a.r
	a.r = r
	a.mu.Unlock()
	if old != nil {
		old.Close() //nolint
	}
}

// Inner returns the current underlying radio (may be nil).
func (a *AtomicRadio) Inner() Radio {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.r
}

func (a *AtomicRadio) Frequency() uint64 {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return 0
	}
	return r.Frequency()
}

func (a *AtomicRadio) SetFrequency(freq uint64) error {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.SetFrequency(freq)
}

func (a *AtomicRadio) PTTOn() error {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.PTTOn()
}

func (a *AtomicRadio) PTTOff() error {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.PTTOff()
}

func (a *AtomicRadio) SplitOn() error {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.SplitOn()
}

func (a *AtomicRadio) SplitOff() error {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.SplitOff()
}

func (a *AtomicRadio) ReadMeters() {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r != nil {
		r.ReadMeters()
	}
}

func (a *AtomicRadio) SMeter() uint32 {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return 0
	}
	return r.SMeter()
}

func (a *AtomicRadio) Power() uint32 {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return 0
	}
	return r.Power()
}

func (a *AtomicRadio) SWR() uint32 {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return 0
	}
	return r.SWR()
}

func (a *AtomicRadio) ALC() uint32 {
	a.mu.RLock()
	r := a.r
	a.mu.RUnlock()
	if r == nil {
		return 0
	}
	return r.ALC()
}

// Close closes the underlying radio if any and sets inner to nil.
func (a *AtomicRadio) Close() error {
	a.Swap(nil)
	return nil
}
