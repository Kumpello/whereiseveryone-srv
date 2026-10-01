package auth

type signUpRequest struct {
	// Username username, must be unique
	Username string `json:"username" validate:"required"`
	// Password user password, min 8 characters
	Password string `json:"password" validate:"required,min=8"`
	// DeviceToken is a required, nonblank client device identifier for single-device auth
	DeviceToken string `json:"device_token" validate:"required"`
}

type logInRequest struct {
	// Username
	Username string `json:"username" validate:"required"`
	// Password user password
	Password string `json:"password" validate:"required"`
	// DeviceToken is a required, nonblank client device identifier for single-device auth
	DeviceToken string `json:"device_token" validate:"required"`
}

type authResponse struct {
	// ID is user id (uuid)
	ID string `json:"id"`
	// Token access token for Bearer authentication; must match the current stored session
	Token string `json:"token"`
	// RefreshToken refresh-only token for /auth/refresh; cannot be used as a Bearer token
	RefreshToken string `json:"refresh_token"`
}

type refreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
	// DeviceToken is required and must match the device bound to the current session
	DeviceToken string `json:"device_token" validate:"required"`
}
