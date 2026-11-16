// Adapted from minecraft/service/signaling in github.com/hashimthearab/gophertunnel
// (commit 9331d09, a fork of github.com/sandertv/gophertunnel), MIT License,
// Copyright (c) 2019 Sandertv. See NOTICE in this folder.

package signaling

import (
	"sync"

	"github.com/google/uuid"
)

// newPendingMap returns a pendingMap ready for use.
func newPendingMap() *pendingMap {
	return &pendingMap{expected: make(map[uuid.UUID]chan error)}
}

// pendingMap tracks pending outbound messages by ID.
// Each registered ID maps to a channel that is completed when the
// corresponding message is completed.
type pendingMap struct {
	expected map[uuid.UUID]chan error
	mu       sync.Mutex
}

// Add registers interest in the completion of the outbound message with
// the given ID and returns a channel that is completed by [pendingMap.Done].
func (i *pendingMap) Add(id uuid.UUID) <-chan error {
	ch := make(chan error, 1)
	i.mu.Lock()
	i.expected[id] = ch
	i.mu.Unlock()
	return ch
}

// Remove stops tracking the outbound message with the given ID and closes
// its channel if it is still registered.
// It is typically deferred after [pendingMap.Add] once waiting for
// completion is no longer needed.
func (i *pendingMap) Remove(id uuid.UUID) {
	i.mu.Lock()
	ch, ok := i.expected[id]
	delete(i.expected, id)
	i.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Done resolves the pending message registered with the given ID.
// It sends err on the associated channel, closes it, and reports whether
// the ID was registered.
func (i *pendingMap) Done(id uuid.UUID, err error) bool {
	i.mu.Lock()
	ch, ok := i.expected[id]
	delete(i.expected, id)
	i.mu.Unlock()
	if ok {
		select {
		case ch <- err:
		default:
		}
		close(ch)
	}
	return ok
}
