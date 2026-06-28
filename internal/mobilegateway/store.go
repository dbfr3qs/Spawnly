// Package mobilegateway is the user-facing edge that lets a mobile app answer
// CIBA spawn-consent prompts. It depends on the platform's consent contract
// (the orchestrator's user-scoped consent endpoints) and never the reverse:
// consent authorization stays at the orchestrator; this package adds only a
// device registry, a push fan-out, and the per-user event stream.
package mobilegateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Device is one registered push target belonging to a single user. PushToken is
// the platform-native token (raw FCM token on Android, APNs device token on
// iOS) the push transport sends to; it is meaningless across users, so every
// store operation is scoped by UserID.
type Device struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Platform  string    `json:"platform"`
	PushToken string    `json:"pushToken"`
	CreatedAt time.Time `json:"createdAt"`
}

// Valid platform values for a registered device.
const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
)

// ValidPlatform reports whether p is a platform the gateway accepts.
func ValidPlatform(p string) bool {
	return p == PlatformIOS || p == PlatformAndroid
}

// DeviceStore persists the user→devices mapping. The interface is context-aware
// and error-returning so a durable backend (DynamoDB/SQL on AWS) can replace the
// in-memory default without touching the handlers — the same Store-interface
// pattern the registry uses (see cmd/registry/store_adapter.go). The in-memory
// implementation ignores ctx and never errors.
type DeviceStore interface {
	// Put inserts or replaces a device. Implementations key by (UserID, ID).
	Put(ctx context.Context, d Device) error
	// Delete removes a device, but only when it belongs to userID. It reports
	// whether a matching device existed — a device owned by another user is
	// reported as not found, never deleted.
	Delete(ctx context.Context, userID, deviceID string) (bool, error)
	// ListByUser returns every device registered to userID (possibly empty).
	ListByUser(ctx context.Context, userID string) ([]Device, error)
}

// ErrInvalidDevice is returned by Put when a device is missing required fields.
var ErrInvalidDevice = errors.New("device missing id, userId, or push token")

// MemoryDeviceStore is the default in-memory DeviceStore: the local-bootstrap
// default and the test double. Concurrency-safe; never persists across a
// restart, which is fine for the dev path (a phone re-registers on next launch).
type MemoryDeviceStore struct {
	mu sync.RWMutex
	// byUser[userID][deviceID] = device. The outer key isolates users so a list
	// or delete can never cross the tenant boundary.
	byUser map[string]map[string]Device
}

// NewMemoryDeviceStore returns an empty in-memory store.
func NewMemoryDeviceStore() *MemoryDeviceStore {
	return &MemoryDeviceStore{byUser: map[string]map[string]Device{}}
}

func (s *MemoryDeviceStore) Put(_ context.Context, d Device) error {
	if d.ID == "" || d.UserID == "" || d.PushToken == "" {
		return ErrInvalidDevice
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, ok := s.byUser[d.UserID]
	if !ok {
		devices = map[string]Device{}
		s.byUser[d.UserID] = devices
	}
	devices[d.ID] = d
	return nil
}

func (s *MemoryDeviceStore) Delete(_ context.Context, userID, deviceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, ok := s.byUser[userID]
	if !ok {
		return false, nil
	}
	if _, ok := devices[deviceID]; !ok {
		return false, nil
	}
	delete(devices, deviceID)
	if len(devices) == 0 {
		delete(s.byUser, userID)
	}
	return true, nil
}

func (s *MemoryDeviceStore) ListByUser(_ context.Context, userID string) ([]Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	devices := s.byUser[userID]
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		out = append(out, d)
	}
	return out, nil
}

// Compile-time proof the in-memory store satisfies the durable-backend contract.
var _ DeviceStore = (*MemoryDeviceStore)(nil)
