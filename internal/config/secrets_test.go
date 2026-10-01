package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"whereiseveryone/pkg/env"
)

func TestJWTSecret(t *testing.T) {
	valid := "a9B2c7D4e6F8g1H3i5J0k9L2m4N6o8P1"
	for _, tt := range []struct {
		name   string
		secret string
		valid  bool
	}{
		{name: "empty"},
		{name: "whitespace", secret: strings.Repeat(" ", 40)},
		{name: "legacy length", secret: "legacy-secret"},
		{name: "too short", secret: valid[:31]},
		{name: "template placeholder", secret: jwtSecretPlaceholder},
		{name: "lowercase placeholder", secret: strings.ToLower(jwtSecretPlaceholder)},
		{name: "leading whitespace", secret: " " + valid},
		{name: "trailing newline", secret: valid + "\n"},
		{name: "32 bytes", secret: valid, valid: true},
		{name: "longer secret", secret: valid + valid, valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(string(ConfJwtSecret), tt.secret)
			handler, err := env.NewOsHandler()
			if err != nil {
				t.Fatal(err)
			}
			secret, err := JWTSecret(handler)
			if tt.valid {
				if err != nil || secret != tt.secret {
					t.Fatal("valid secret was rejected or modified")
				}
			} else {
				if err == nil || secret != "" {
					t.Fatal("invalid secret was accepted or returned")
				}
				if tt.secret != "" && strings.Contains(err.Error(), tt.secret) {
					t.Fatal("error exposes the rejected secret")
				}
			}
		})
	}
	t.Run("missing JSON key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		handler, err := env.NewJSONHandler(path)
		if err != nil {
			t.Fatal(err)
		}
		if secret, err := JWTSecret(handler); err == nil || secret != "" {
			t.Fatal("missing secret was accepted")
		}
	})
}

func TestCreateFromTemplate(t *testing.T) {
	dir := t.TempDir()
	secrets := make(map[string]bool)
	for _, name := range []string{"local", "cloud", "docker"} {
		t.Run(name, func(t *testing.T) {
			template := filepath.Join("..", "..", ".env", name+".example.json")
			handler, err := env.NewJSONHandler(template)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := JWTSecret(handler); err == nil {
				t.Fatal("unmodified template must be rejected at startup")
			}
			output := filepath.Join(dir, name+".json")
			if err := CreateFromTemplate(template, output); err != nil {
				t.Fatal(err)
			}
			handler, err = env.NewJSONHandler(output)
			if err != nil {
				t.Fatal(err)
			}
			secret, err := JWTSecret(handler)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.RawStdEncoding.DecodeString(secret)
			if err != nil || len(decoded) != 32 || secrets[secret] {
				t.Fatal("expected an independent 256-bit generated secret")
			}
			secrets[secret] = true
			info, err := os.Stat(output)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("configuration permissions = %o; want 600", info.Mode().Perm())
			}
			original, err := os.ReadFile(template)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]string
			if err := json.Unmarshal(original, &values); err != nil {
				t.Fatal(err)
			}
			for key, value := range values {
				if key != string(ConfJwtSecret) && handler.Env(env.Key(key), "") != value {
					t.Fatalf("configuration key %s was modified", key)
				}
			}
			if err := CreateFromTemplate(template, output); err == nil {
				t.Fatal("existing configuration was overwritten")
			}
			after, err := env.NewJSONHandler(output)
			if err != nil {
				t.Fatal(err)
			}
			if after.Env(ConfJwtSecret, "") != secret {
				t.Fatal("existing secret changed")
			}
		})
	}
}

func TestCreateFromTemplateRejectsInvalidJSON(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `{"app.port":8080}`, `not-json`} {
		t.Run(input, func(t *testing.T) {
			dir := t.TempDir()
			template, output := filepath.Join(dir, "template.json"), filepath.Join(dir, "output.json")
			if err := os.WriteFile(template, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CreateFromTemplate(template, output); err == nil {
				t.Fatal("invalid template was accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid template created an output file")
			}
		})
	}
}
