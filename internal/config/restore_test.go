package config

import (
	"testing"

	"whereiseveryone/pkg/env"
)

func TestRestoreDefaultTrustsProductionCertificateOnly(t *testing.T) {
	settings, err := RestoreCredentialsFromEnv(restoreEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if settings.RPID != "where-is-everyone.com" || len(settings.AndroidOrigins) != 2 {
		t.Fatalf("unexpected default relying party: %+v", settings)
	}
	if len(settings.AndroidOrigins["com.kumpello.whereiseveryone"]) != 1 {
		t.Fatal("production app certificate missing")
	}
	if _, present := settings.AndroidOrigins["com.kumpello.whereiseveryone.debug"]; present {
		t.Fatal("production defaults trust debug builds")
	}
	disabled, err := RestoreCredentialsFromEnv(restoreEnv{ConfRestoreAndroidOrigins: "{}"})
	if err != nil || len(disabled.AndroidOrigins) != 0 {
		t.Fatalf("empty allowlist did not disable restore: %v", err)
	}
}

func TestRestoreConfigurationRejectsUntrustedOrigins(t *testing.T) {
	for _, origins := range []string{
		`["android:apk-key-hash:hash"]`, `{"app":[]}`, `{" ":["android:apk-key-hash:hash"]}`,
		`{"app":["https://example.com"]}`, `{"app":["*"]}`, `{"app":["android:apk-key-hash:short"]}`,
	} {
		if _, err := RestoreCredentialsFromEnv(restoreEnv{ConfRestoreAndroidOrigins: origins}); err == nil {
			t.Fatalf("accepted invalid allowlist: %s", origins)
		}
	}
	if _, err := RestoreCredentialsFromEnv(restoreEnv{ConfRestoreRPID: "https://example.com"}); err == nil {
		t.Fatal("accepted an RP ID containing a scheme")
	}
}

func (e restoreEnv) Env(key env.Key, fallback string) string {
	if value, ok := e[key]; ok {
		return value
	}
	return fallback
}

func (e restoreEnv) MustEnv(key env.Key) string { return e[key] }

type restoreEnv map[env.Key]string
