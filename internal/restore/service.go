// Package restore verifies system-managed Android restore keys, not user-facing passkeys.
package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"whereiseveryone/internal/config"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/timer"
)

// Server-enforced expiry periods for ceremonies and inactive restore keys.
const (
	ChallengeLifetime = 5 * time.Minute
	KeyLifetime       = 365 * 24 * time.Hour
	registration      = "registration"
	authentication    = "authentication"
)

var (
	// ErrRejected hides credential, ownership and challenge details from callers.
	ErrRejected = errors.New("restore credential rejected")
	// ErrDisabled indicates an application outside the configured allowlist.
	ErrDisabled = errors.New("restore credentials unavailable for this app")
)

// NewService prepares a verifier for each trusted Android application.
func NewService(settings config.RestoreCredentials, store Store, clock timer.Timer) (*Service, error) {
	apps := make(map[string]*webauthn.WebAuthn, len(settings.AndroidOrigins))
	for app, origins := range settings.AndroidOrigins {
		instance, err := webauthn.New(&webauthn.Config{
			RPID: settings.RPID, RPDisplayName: "Where Is Everyone",
			RPOrigins: []string{"https://" + settings.RPID}, RPOpaqueOrigins: origins,
			AttestationPreference: protocol.PreferNoAttestation,
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationDiscouraged,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("configure restore credentials: %w", err)
		}
		apps[app] = instance
	}
	return &Service{apps: apps, store: store, clock: clock}, nil
}

// BeginRegistration binds creation options to an authenticated account/device.
func (s *Service) BeginRegistration(ctx context.Context, userID id.ID, username, app, device string) (Options, error) {
	instance, ok := s.apps[app]
	if !ok {
		return Options{}, ErrDisabled
	}
	options, session, err := instance.BeginRegistration(restoreUser{userID, username})
	if err != nil {
		return Options{}, fmt.Errorf("create registration options: %w", err)
	}
	return s.saveChallenge(ctx, app, device, userID, registration, *session, options.Response)
}

// FinishRegistration verifies and persists an account-owned restore key.
func (s *Service) FinishRegistration(ctx context.Context, userID id.ID,
	app, device, challengeID, response string,
) error {
	instance, ok := s.apps[app]
	if !ok {
		return ErrDisabled
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes([]byte(response))
	if err != nil {
		return ErrRejected
	}
	// Retrying an acknowledged registration after a lost HTTP response is safe:
	// it must be byte-identical and belong to this authenticated account/device.
	previous, err := s.store.FindKey(ctx, app, parsed.RawID, s.clock.Now())
	if err == nil && previous.UserID == userID && previous.DeviceDigest == digest(device) &&
		previous.RegistrationDigest == digest(challengeID+"\x00"+response) {
		return nil
	}
	if err != nil && !errors.Is(err, ErrRejected) {
		return fmt.Errorf("check registered restore key: %w", err)
	}
	challenge, err := s.store.TakeChallenge(ctx, challengeID, registration, app, digest(device), userID, s.clock.Now())
	if err != nil {
		return fmt.Errorf("consume registration challenge: %w", err)
	}
	if !bytes.Equal(challenge.Session.UserID, []byte(userID.Hex())) {
		return ErrRejected
	}
	configuration := instance.Config
	// Restore keys intentionally run without user presence/verification. Use the
	// library's protocol verifier; its normal login API always requires presence.
	clientHash, err := parsed.Verify(challenge.Session.Challenge, configuration.RPID,
		nil, configuration.RPOpaqueOrigins, nil, protocol.TopOriginExplicitVerificationMode,
		false, false, false, nil, challenge.Session.CredParams, configuration.Attestation, configuration.Signature)
	if err != nil || !validBackupFlags(parsed.Response.AttestationObject.AuthData.Flags) {
		return ErrRejected
	}
	if err = parsed.ClientExtensionResults.Verify(challenge.Session.Extensions, protocol.CreateCeremony,
		protocol.UnsolicitedOutputPolicyReject); err != nil {
		return ErrRejected
	}
	if err = parsed.Response.AttestationObject.AuthData.Ext.Verify(
		challenge.Session.Extensions, protocol.CreateCeremony); err != nil {
		return ErrRejected
	}
	credential, err := webauthn.NewCredential(clientHash, parsed)
	if err != nil {
		return ErrRejected
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("encode restore credential: %w", err)
	}
	now := s.clock.Now()
	key := Key{
		ID: keyID(app, credential.ID), CredentialID: credential.ID, UserID: userID, AppID: app,
		DeviceDigest: digest(device), CredentialJSON: encoded,
		RegistrationDigest: digest(challengeID + "\x00" + response),
		LastUsedAt:         now, ExpiresAt: now.Add(KeyLifetime),
	}
	if err = s.store.SaveKey(ctx, key); err != nil {
		return fmt.Errorf("persist restore key: %w", err)
	}
	return nil
}

// BeginAuthentication issues an account-independent challenge for a new device.
func (s *Service) BeginAuthentication(ctx context.Context, app, device string) (Options, error) {
	instance, ok := s.apps[app]
	if !ok {
		return Options{}, ErrDisabled
	}
	options, session, err := instance.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationDiscouraged))
	if err != nil {
		return Options{}, fmt.Errorf("create authentication options: %w", err)
	}
	return s.saveChallenge(ctx, app, device, id.ZeroID, authentication, *session, options.Response)
}

// FinishAuthentication verifies a silent assertion and returns its account owner.
func (s *Service) FinishAuthentication(ctx context.Context, app, device, challengeID, response string) (id.ID, error) {
	instance, ok := s.apps[app]
	if !ok {
		return id.ZeroID, ErrDisabled
	}
	challenge, err := s.store.TakeChallenge(
		ctx, challengeID, authentication, app, digest(device), id.ZeroID, s.clock.Now())
	if err != nil {
		return id.ZeroID, fmt.Errorf("consume authentication challenge: %w", err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(response))
	if err != nil {
		return id.ZeroID, ErrRejected
	}
	key, err := s.store.FindKey(ctx, app, parsed.RawID, s.clock.Now())
	if err != nil {
		return id.ZeroID, fmt.Errorf("load restore key: %w", err)
	}
	if !bytes.Equal(parsed.Response.UserHandle, []byte(key.UserID.Hex())) {
		return id.ZeroID, ErrRejected
	}
	var credential webauthn.Credential
	if err = json.Unmarshal(key.CredentialJSON, &credential); err != nil {
		return id.ZeroID, errors.New("decode stored restore credential")
	}
	configuration := instance.Config
	if err = parsed.Verify(challenge.Session.Challenge, configuration.RPID, "",
		nil, configuration.RPOpaqueOrigins, nil, protocol.TopOriginExplicitVerificationMode,
		false, false, false, credential.PublicKey, configuration.Signature); err != nil {
		return id.ZeroID, ErrRejected
	}
	flags := parsed.Response.AuthenticatorData.Flags
	if !validBackupFlags(flags) || credential.Flags.BackupEligible != flags.HasBackupEligible() {
		return id.ZeroID, ErrRejected
	}
	if err = parsed.ClientExtensionResults.Verify(challenge.Session.Extensions, protocol.AssertCeremony,
		protocol.UnsolicitedOutputPolicyReject); err != nil {
		return id.ZeroID, ErrRejected
	}
	if err = parsed.Response.AuthenticatorData.Ext.Verify(
		challenge.Session.Extensions, protocol.AssertCeremony); err != nil {
		return id.ZeroID, ErrRejected
	}
	count := parsed.Response.AuthenticatorData.Counter
	// Synced copies can have independent counters. One-use server challenges are
	// the replay defence; a device-bound, nonzero counter must still advance.
	storedCount := credential.Authenticator.SignCount
	if !flags.HasBackupEligible() && storedCount != 0 && count <= storedCount {
		return id.ZeroID, ErrRejected
	}
	if count > credential.Authenticator.SignCount {
		credential.Authenticator.SignCount = count
	}
	credential.Flags = credential.Flags.Update(flags)
	encoded, err := json.Marshal(credential)
	if err != nil {
		return id.ZeroID, fmt.Errorf("encode updated restore credential: %w", err)
	}
	if err = s.store.UseKey(ctx, key, encoded, s.clock.Now()); err != nil {
		return id.ZeroID, fmt.Errorf("update restore key: %w", err)
	}
	return key.UserID, nil
}

func (s *Service) saveChallenge(ctx context.Context, app, device string, userID id.ID, purpose string,
	session webauthn.SessionData, options any,
) (Options, error) {
	encoded, err := json.Marshal(options)
	if err != nil {
		return Options{}, fmt.Errorf("encode restore options: %w", err)
	}
	challenge := Challenge{ID: rand.Text(), Purpose: purpose, AppID: app, DeviceDigest: digest(device),
		UserID: userID, Session: session, ExpiresAt: s.clock.Now().Add(ChallengeLifetime)}
	if err = s.store.SaveChallenge(ctx, challenge); err != nil {
		return Options{}, fmt.Errorf("persist restore challenge: %w", err)
	}
	return Options{ChallengeID: challenge.ID, RequestJSON: string(encoded)}, nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func keyID(app string, credentialID []byte) string {
	return digest(app + "\x00" + base64.RawURLEncoding.EncodeToString(credentialID))
}

func validBackupFlags(flags protocol.AuthenticatorFlags) bool {
	return flags.HasBackupEligible() || !flags.HasBackupState()
}

func (u restoreUser) WebAuthnID() []byte                         { return []byte(u.id.Hex()) }
func (u restoreUser) WebAuthnName() string                       { return u.name }
func (u restoreUser) WebAuthnDisplayName() string                { return u.name }
func (u restoreUser) WebAuthnCredentials() []webauthn.Credential { return nil }

// Service handles system-managed key ceremonies with explicit silent verification.
type Service struct {
	apps  map[string]*webauthn.WebAuthn
	store Store
	clock timer.Timer
}

// Options contains a server challenge reference and the platform WebAuthn JSON.
type Options struct {
	ChallengeID string `json:"challenge_id"`
	RequestJSON string `json:"request_json"`
}

type restoreUser struct {
	id   id.ID
	name string
}
