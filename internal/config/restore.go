package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"whereiseveryone/pkg/env"
)

// Restore credential configuration keys; defaults trust signed production apps.
const (
	ConfRestoreRPID           env.Key = "app.restoreRPID"
	ConfRestoreAndroidOrigins env.Key = "app.restoreAndroidOrigins"
	// Public certificate fingerprint from the signed production APK. Override
	// the allowlist for Play App Signing or certificate rotation, never wildcard it.
	defaultRestoreOrigins = `{
"com.kumpello.whereiseveryone": [
"android:apk-key-hash:FZ8-huqbfDmzEVV-OxwPrelm4SCHu-7sw6XK_Xd-4qs"
],
"com.kumpello.whereiseveryone.premium": [
"android:apk-key-hash:FZ8-huqbfDmzEVV-OxwPrelm4SCHu-7sw6XK_Xd-4qs"
]
}`
)

// RestoreCredentialsFromEnv reads the trusted package/certificate allowlist.
// An empty allowlist disables restoration without affecting password sign-in.
func RestoreCredentialsFromEnv(handler env.Handler) (RestoreCredentials, error) {
	settings := RestoreCredentials{RPID: handler.Env(ConfRestoreRPID, "where-is-everyone.com")}
	if err := protocol.ValidateRPID(settings.RPID); err != nil {
		return RestoreCredentials{}, fmt.Errorf("invalid app.restoreRPID: %w", err)
	}
	originsJSON := handler.Env(ConfRestoreAndroidOrigins, defaultRestoreOrigins)
	if err := json.Unmarshal([]byte(originsJSON), &settings.AndroidOrigins); err != nil {
		return RestoreCredentials{}, errors.New("app.restoreAndroidOrigins must map packages to certificate origins")
	}
	for app, origins := range settings.AndroidOrigins {
		if strings.TrimSpace(app) != app || app == "" || len(app) > 256 || len(origins) == 0 {
			return RestoreCredentials{}, errors.New("restore packages must be nonblank and have a certificate origin")
		}
		for _, origin := range origins {
			const prefix = "android:apk-key-hash:"
			if !strings.HasPrefix(origin, prefix) {
				return RestoreCredentials{}, errors.New("restore origins must use android:apk-key-hash")
			}
			digest, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(origin, prefix))
			if err != nil || len(digest) != 32 {
				return RestoreCredentials{}, errors.New("restore origin hashes must be unpadded base64url SHA-256 digests")
			}
		}
	}
	return settings, nil
}

// RestoreCredentials describes trusted Android applications, independently of passkeys.
type RestoreCredentials struct {
	RPID           string
	AndroidOrigins map[string][]string
}
