package restore

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"whereiseveryone/internal/config"
	"whereiseveryone/pkg/id"
)

func TestMongoRestorationPersistsAcrossServerInstances(t *testing.T) {
	store := mongoTestStore(t)
	_, _, clock := newTestService(t)
	settings := config.RestoreCredentials{RPID: testRPID, AndroidOrigins: map[string][]string{testApp: {testOrigin}}}
	first, err := NewService(settings, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	userID := id.NewID()
	key := newVirtualKey(t, userID)
	enroll(t, first, key, userID, "source")
	options, err := first.BeginAuthentication(t.Context(), testApp, "destination")
	if err != nil {
		t.Fatal(err)
	}
	// A second process can complete a challenge issued by the first process.
	second, err := NewService(settings, NewMongoStore(store.keys.Database()), clock)
	if err != nil {
		t.Fatal(err)
	}
	response := key.assertion(t, challengeValue(t, options), testOrigin, testRPID, 0, userID)
	restored, err := second.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID, response)
	if err != nil || restored != userID {
		t.Fatalf("persisted restoration failed: %v", err)
	}
	if _, replayErr := first.FinishAuthentication(t.Context(), testApp, "destination", options.ChallengeID, response); !errors.Is(replayErr, ErrRejected) {
		t.Fatalf("persisted challenge replay accepted: %v", replayErr)
	}
	stored, err := store.FindKey(t.Context(), testApp, key.credentialID, clock.Now())
	if err != nil || stored.Revision != 1 {
		t.Fatalf("credential updates did not persist: revision %d, error %v", stored.Revision, err)
	}
}

func TestMongoChallengeConsumptionIsAtomicAndBound(t *testing.T) {
	store := mongoTestStore(t)
	now := time.Now().UTC()
	userID := id.NewID()
	challenge := Challenge{ID: "challenge", Purpose: registration, AppID: testApp,
		DeviceDigest: "device", UserID: userID, ExpiresAt: now.Add(ChallengeLifetime)}
	if err := store.SaveChallenge(t.Context(), challenge); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakeChallenge(t.Context(), challenge.ID, registration, testApp, "other-device", userID, now); !errors.Is(err, ErrRejected) {
		t.Fatalf("wrong device consumed challenge: %v", err)
	}
	results := make(chan error, 4)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			_, err := store.TakeChallenge(t.Context(), challenge.ID, registration, testApp, "device", userID, now)
			results <- err
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
		t.Fatalf("challenge consumed %d times", successes)
	}
}

func TestMongoKeyLimitsKeepNewKeyAndAllowMultipleDevices(t *testing.T) {
	store := mongoTestStore(t)
	now := time.Now().UTC()
	userID := id.NewID()
	makeKey := func(device string) Key {
		credentialID := []byte(id.NewID().Hex())
		return Key{ID: keyID(testApp, credentialID), AppID: testApp, UserID: userID,
			CredentialID: credentialID, DeviceDigest: device, CredentialJSON: []byte("record"), LastUsedAt: now,
			ExpiresAt: now.Add(KeyLifetime)}
	}
	for range maxKeysPerDevice + 3 {
		key := makeKey("source")
		if err := store.SaveKey(t.Context(), key); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FindKey(t.Context(), testApp, key.CredentialID, now); err != nil {
			t.Fatalf("cap pruned the newly enrolled key: %v", err)
		}
	}
	count, err := store.keys.CountDocuments(t.Context(), bson.M{"user_id": userID, "device_digest": "source"})
	if err != nil || count != maxKeysPerDevice {
		t.Fatalf("device key cap failed: %d, %v", count, err)
	}
	for range maxKeysPerAccount {
		if saveErr := store.SaveKey(t.Context(), makeKey(id.NewID().Hex())); saveErr != nil {
			t.Fatal(saveErr)
		}
	}
	count, err = store.keys.CountDocuments(t.Context(), bson.M{"user_id": userID})
	if err != nil || count != maxKeysPerAccount {
		t.Fatalf("account key cap failed: %d, %v", count, err)
	}
	key := makeKey("destination")
	if err := store.SaveKey(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if err := store.UseKey(t.Context(), key, key.CredentialJSON, now); err != nil {
		t.Fatal(err)
	}
	if err := store.UseKey(t.Context(), key, key.CredentialJSON, now); !errors.Is(err, ErrRejected) {
		t.Fatalf("stale credential update was accepted: %v", err)
	}
}

func mongoTestStore(t *testing.T) *MongoStore {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run restore persistence regression tests")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("restore_regression_" + id.NewID().Hex())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.Drop(ctx); err != nil {
			t.Error(err)
		}
		if err := client.Disconnect(ctx); err != nil {
			t.Error(err)
		}
	})
	store := NewMongoStore(db)
	if err := store.EnsureIndexes(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}
