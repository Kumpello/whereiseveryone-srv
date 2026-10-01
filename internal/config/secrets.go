package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"whereiseveryone/pkg/env"
)

const jwtSecretPlaceholder = "REPLACE_WITH_A_RANDOM_SECRET_GENERATED_FOR_THIS_ENVIRONMENT"

// JWTSecret validates configuration before any external services are contacted.
// Length is a minimum safeguard, not proof of entropy; provision random secrets.
func JWTSecret(handler env.Handler) (string, error) {
	secret := handler.Env(ConfJwtSecret, "")
	if len(strings.TrimSpace(secret)) < 32 || secret != strings.TrimSpace(secret) ||
		strings.EqualFold(secret, jwtSecretPlaceholder) {
		return "", errors.New("app.jwtSecret must be an independently generated secret of at least 32 bytes, without surrounding whitespace; run cli initConfig to generate a configuration")
	}
	return secret, nil
}

// CreateFromTemplate provisions a new configuration with 256 bits of random
// signing-key material. Existing files (including symlinks) are never replaced.
func CreateFromTemplate(templatePath, outputPath string) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read configuration template: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		// Decoder errors can include values from the file; do not log them.
		return errors.New("configuration template must be a JSON object with string values")
	}
	if values == nil {
		return errors.New("configuration template must be a JSON object with string values")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return fmt.Errorf("generate JWT secret: %w", err)
	}
	values[string(ConfJwtSecret)] = base64.RawStdEncoding.EncodeToString(secret[:])
	data, err = json.MarshalIndent(values, "", "  ")
	if err != nil {
		return errors.New("encode generated configuration")
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create configuration (existing files are never overwritten): %w", err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	return nil
}
