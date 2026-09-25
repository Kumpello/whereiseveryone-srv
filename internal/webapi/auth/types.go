package auth

type signUpRequest struct {
	// Username username, must be unique
	Username string `json:"username" validate:"required"`
	// Password user password, min 8 characters
	Password string `json:"password" validate:"required,min=8"`
	// DeviceToken identifies the client device for single-device auth
	DeviceToken string `json:"device_token" validate:"omitempty"`
}

type logInRequest struct {
	// Username
	Username string `json:"username" validate:"required"`
	// Password user password
	Password string `json:"password" validate:"required"`
	// DeviceToken identifies the client device for single-device auth
	DeviceToken string `json:"device_token" validate:"omitempty"`
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
	// DeviceToken identifies the client device requesting refresh
	DeviceToken string `json:"device_token" validate:"omitempty"`
}
