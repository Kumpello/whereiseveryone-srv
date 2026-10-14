package restore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"whereiseveryone/internal/config"
	"whereiseveryone/pkg/id"
)

const testApp = "com.example.restore"
const testRPID = "example.com"
const testOrigin = "android:apk-key-hash:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestRestoreWithoutUserPresenceAndAssertionReplay(t *testing.T) {
	service, store, clock := newTestService(t)
	userID := id.NewID()
	key := newVirtualKey(t, userID)
	enroll(t, service, key, userID, "source")
	// Registration retries after a dropped response must not consume another key.
	if len(store.keys) != 1 {
		t.Fatal("registration did not persist exactly one key")
	}
	options, err := service.BeginAuthentication(t.Context(), testApp, "destination")
	if err != nil {
		t.Fatal(err)
	}
	response := key.assertion(t, challengeValue(t, options), testOrigin, testRPID, 0, userID)
	got, err := service.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID, response)
	if err != nil || got != userID {
		t.Fatalf("silent restore failed: user %s, error %v", got.Hex(), err)
	}
	if _, err := service.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID, response); !errors.Is(err, ErrRejected) {
		t.Fatalf("replayed assertion accepted: %v", err)
	}
	stored := store.keys[keyID(testApp, key.credentialID)]
	if !stored.ExpiresAt.Equal(clock.Now().Add(KeyLifetime)) {
		t.Fatal("using a key did not renew inactivity expiry")
	}
}

func TestRestoreRejectsTamperedAssertions(t *testing.T) {
	for _, scenario := range []string{"challenge", "origin", "rp", "signature", "owner", "device", "expiry", "app"} {
		t.Run(scenario, func(t *testing.T) {
			service, _, clock := newTestService(t)
			userID := id.NewID()
			key := newVirtualKey(t, userID)
			enroll(t, service, key, userID, "source")
			options, err := service.BeginAuthentication(t.Context(), testApp, "destination")
			if err != nil {
				t.Fatal(err)
			}
			challenge, origin, rp, device, app, owner := challengeValue(t, options), testOrigin, testRPID, "destination", testApp, userID
			switch scenario {
			case "challenge":
				challenge = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			case "origin":
				origin = "android:apk-key-hash:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
			case "rp":
				rp = "attacker.example"
			case "signature":
				key.private = newVirtualKey(t, userID).private
			case "owner":
				owner = id.NewID()
			case "device":
				device = "other-device"
			case "expiry":
				clock.now = clock.now.Add(ChallengeLifetime)
			case "app":
				app = "com.example.unknown"
			}
			response := key.assertion(t, challenge, origin, rp, 0, owner)
			if _, err := service.FinishAuthentication(t.Context(), app, device, options.ChallengeID, response); err == nil {
				t.Fatalf("accepted tampered %s", scenario)
			}
		})
	}
}

func TestRegistrationChallengeBelongsToAuthenticatedAccount(t *testing.T) {
	service, _, _ := newTestService(t)
	userID := id.NewID()
	key := newVirtualKey(t, userID)
	options, err := service.BeginRegistration(t.Context(), userID, "alice", testApp, "source")
	if err != nil {
		t.Fatal(err)
	}
	response := key.registration(t, challengeValue(t, options), testOrigin)
	if err := service.FinishRegistration(t.Context(), id.NewID(), testApp, "source", options.ChallengeID, response); !errors.Is(err, ErrRejected) {
		t.Fatalf("another account enrolled a key: %v", err)
	}
	if err := service.FinishRegistration(t.Context(), userID, testApp, "source", options.ChallengeID, response); err != nil {
		t.Fatal(err)
	}
	if err := service.FinishRegistration(t.Context(), userID, testApp, "source", options.ChallengeID, response); err != nil {
		t.Fatalf("identical registration retry failed: %v", err)
	}
}

func TestConcurrentAssertionOnlyConsumesChallengeOnce(t *testing.T) {
	service, _, _ := newTestService(t)
	userID := id.NewID()
	key := newVirtualKey(t, userID)
	enroll(t, service, key, userID, "source")
	options, err := service.BeginAuthentication(t.Context(), testApp, "destination")
	if err != nil {
		t.Fatal(err)
	}
	response := key.assertion(t, challengeValue(t, options), testOrigin, testRPID, 0, userID)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, authErr := service.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID, response)
			results <- authErr
		})
	}
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrRejected) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("accepted %d simultaneous assertions", successes)
	}
}

func TestExpiredKeysAndDifferentPackagesDoNotRestore(t *testing.T) {
	service, _, clock := newTestService(t)
	userID := id.NewID()
	key := newVirtualKey(t, userID)
	enroll(t, service, key, userID, "source")
	clock.now = clock.now.Add(KeyLifetime)
	options, err := service.BeginAuthentication(t.Context(), testApp, "destination")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID,
		key.assertion(t, challengeValue(t, options), testOrigin, testRPID, 0, userID)); !errors.Is(err, ErrRejected) {
		t.Fatalf("expired key accepted: %v", err)
	}
	if _, err := service.BeginAuthentication(t.Context(), "unknown.package", "destination"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("untrusted app accepted: %v", err)
	}
}

func TestCountersAllowSyncedCopiesButRejectDeviceBoundRegression(t *testing.T) {
	for _, backupEligible := range []bool{true, false} {
		name := "device bound"
		if backupEligible {
			name = "synced"
		}
		t.Run(name, func(t *testing.T) {
			service, store, _ := newTestService(t)
			userID := id.NewID()
			key := newVirtualKey(t, userID)
			key.backupEligible = backupEligible
			enroll(t, service, key, userID, "second-source")
			for _, count := range []uint32{10, 1} {
				options, err := service.BeginAuthentication(t.Context(), testApp, "destination")
				if err != nil {
					t.Fatal(err)
				}
				_, err = service.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID,
					key.assertion(t, challengeValue(t, options), testOrigin, testRPID, count, userID))
				if count == 1 && !backupEligible {
					if !errors.Is(err, ErrRejected) {
						t.Fatalf("device-bound counter regression accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if len(store.keys) != 1 {
				t.Fatal("counter updates changed key ownership")
			}
		})
	}
}

func newTestService(t *testing.T) (*Service, *memoryStore, *testClock) {
	t.Helper()
	clock := &testClock{time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	store := &memoryStore{challenges: map[string]Challenge{}, keys: map[string]Key{}}
	service, err := NewService(config.RestoreCredentials{
		RPID: testRPID, AndroidOrigins: map[string][]string{testApp: {testOrigin}},
	}, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, clock
}

func enroll(t *testing.T, service *Service, key virtualKey, userID id.ID, device string) {
	t.Helper()
	options, err := service.BeginRegistration(t.Context(), userID, "alice", testApp, device)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.FinishRegistration(t.Context(), userID, testApp, device, options.ChallengeID,
		key.registration(t, challengeValue(t, options), testOrigin)); err != nil {
		t.Fatal(err)
	}
}

func challengeValue(t *testing.T, options Options) string {
	t.Helper()
	var values struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(options.RequestJSON), &values); err != nil {
		t.Fatal(err)
	}
	return values.Challenge
}

func newVirtualKey(t *testing.T, userID id.ID) virtualKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return virtualKey{private: private, credentialID: []byte("credential-" + userID.Hex()), backupEligible: true}
}

func (k virtualKey) registration(t *testing.T, challenge, origin string) string {
	t.Helper()
	public, err := cbor.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1, -2: k.private.X.FillBytes(make([]byte, 32)), -3: k.private.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	flags := byte(0x40) // Attested; no UP/UV.
	if k.backupEligible {
		flags |= 0x08
	}
	data := authData(testRPID, flags, 0)
	data = append(data, make([]byte, 16)...)
	// Credential IDs in this fixture are "credential-" + a 24-byte Mongo ID.
	data = binary.BigEndian.AppendUint16(data, 35)
	data = append(data, k.credentialID...)
	data = append(data, public...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": data})
	if err != nil {
		t.Fatal(err)
	}
	return k.response(t, map[string]string{
		"clientDataJSON": encode(clientData(t, "webauthn.create", challenge, origin)), "attestationObject": encode(attestation),
	})
}

func (k virtualKey) assertion(t *testing.T, challenge, origin, rp string, count uint32, owner id.ID) string {
	t.Helper()
	client := clientData(t, "webauthn.get", challenge, origin)
	flags := byte(0) // Deliberately no UP/UV.
	if k.backupEligible {
		flags |= 0x08
	}
	data := authData(rp, flags, count)
	clientHash := sha256.Sum256(client)
	signed := sha256.Sum256(append(append([]byte{}, data...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, k.private, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return k.response(t, map[string]string{"clientDataJSON": encode(client), "authenticatorData": encode(data),
		"signature": encode(signature), "userHandle": encode([]byte(owner.Hex()))})
}

func (k virtualKey) response(t *testing.T, response map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"id": encode(k.credentialID), "rawId": encode(k.credentialID),
		"type": "public-key", "response": response})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func clientData(t *testing.T, kind, challenge, origin string) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func authData(rp string, flags byte, count uint32) []byte {
	rpHash := sha256.Sum256([]byte(rp))
	return binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), flags), count)
}

func encode(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func (c *testClock) Now() time.Time { return c.now }

func (s *memoryStore) SaveChallenge(_ context.Context, challenge Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challenges[challenge.ID] = challenge
	return nil
}

func (s *memoryStore) TakeChallenge(_ context.Context, challengeID, purpose, app, device string,
	userID id.ID, now time.Time,
) (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	challenge, ok := s.challenges[challengeID]
	if !ok || challenge.Purpose != purpose || challenge.AppID != app || challenge.DeviceDigest != device ||
		challenge.UserID != userID || !challenge.ExpiresAt.After(now) {
		return Challenge{}, ErrRejected
	}
	delete(s.challenges, challengeID)
	return challenge, nil
}

func (s *memoryStore) FindKey(_ context.Context, app string, credentialID []byte, now time.Time) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[keyID(app, credentialID)]
	if !ok || !key.ExpiresAt.After(now) {
		return Key{}, ErrRejected
	}
	return key, nil
}

func (s *memoryStore) SaveKey(_ context.Context, key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[key.ID]; exists {
		return ErrRejected
	}
	s.keys[key.ID] = key
	return nil
}

func (s *memoryStore) UseKey(_ context.Context, previous Key, encoded []byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, exists := s.keys[previous.ID]
	if !exists || key.Revision != previous.Revision || !key.ExpiresAt.After(now) {
		return ErrRejected
	}
	key.CredentialJSON, key.ExpiresAt, key.LastUsedAt = encoded, now.Add(KeyLifetime), now
	key.Revision++
	s.keys[key.ID] = key
	return nil
}

type testClock struct{ now time.Time }
type virtualKey struct {
	private        *ecdsa.PrivateKey
	credentialID   []byte
	backupEligible bool
}
type memoryStore struct {
	mu         sync.Mutex
	challenges map[string]Challenge
	keys       map[string]Key
}
