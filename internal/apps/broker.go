package apps

import "sync"

// Event is what the SSE endpoint streams to the list page.
type Event struct {
	Type string `json:"type"` // "app" (changed) or "deleted"
	ID   string `json:"id"`
	// OwnerID is set on deletions (the app is gone, so the view cannot
	// carry it); the admin feed needs it.
	OwnerID string `json:"owner_id,omitempty"`
	App     *View  `json:"app,omitempty"`
}

// AllOwners subscribes to every owner's events (the admin overview).
const AllOwners = "*"

// Broker fans app events out to SSE subscribers, per owner.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]string
}

// NewBroker returns an empty broker.
func NewBroker() *Broker { return &Broker{subs: map[chan Event]string{}} }

// Subscribe returns a channel of events for ownerID and a cancel func.
func (b *Broker) Subscribe(ownerID string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = ownerID
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish delivers ev to every subscriber of ownerID. Slow subscribers
// miss events; the list page reloads on reconnect anyway.
func (b *Broker) Publish(ownerID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, owner := range b.subs {
		if owner != ownerID && owner != AllOwners {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
}
