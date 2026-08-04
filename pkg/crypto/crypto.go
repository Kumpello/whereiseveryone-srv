package crypto

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const DefaultPasswordHashCost = 14

func HashPassword(password string) (string, error) {
	return HashPasswordWithCost(password, DefaultPasswordHashCost)
}

func HashPasswordWithCost(password string, cost int) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("encrypt the password: %w", err)
	}

	return string(bytes), nil
}

// VerifyPassword compares user password with the encrypted one
func VerifyPassword(userPassword string, providedPassword string) error {
	err := bcrypt.CompareHashAndPassword([]byte(userPassword), []byte(providedPassword))

	if err != nil {
		return fmt.Errorf("incorrect password: %w", err)
	}

	return nil
}
