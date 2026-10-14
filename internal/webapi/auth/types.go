package auth

type signUpRequest struct {
	// Username must be unique and at most 64 characters
	Username string `json:"username" validate:"required,max=64"`
	// Password is at least 8 characters and at most 72 bytes
	Password string `json:"password" validate:"required,min=8,max=72"`
	// DeviceToken is a nonblank device identifier of at most 256 characters
	DeviceToken string `json:"device_token" validate:"required,max=256"`
}

type logInRequest struct {
	// Username is at most 64 characters
	Username string `json:"username" validate:"required,max=64"`
	// Password is at most 72 bytes
	Password string `json:"password" validate:"required,max=72"`
	// DeviceToken is a nonblank device identifier of at most 256 characters
	DeviceToken string `json:"device_token" validate:"required,max=256"`
}

type authResponse struct {
	// ID is the user's MongoDB ObjectID
	ID string `json:"id"`
	// Token is the current access token for Bearer authentication
	Token string `json:"token"`
	// RefreshToken is used only by /auth/refresh, not Bearer authentication
	RefreshToken string `json:"refresh_token"`
}

type refreshTokenRequest struct {
	// RefreshToken is an encoded JWT of at most 4096 bytes
	RefreshToken string `json:"refresh_token" validate:"required,max=4096"`
	// DeviceToken must match the session device; at most 256 characters
	DeviceToken string `json:"device_token" validate:"required,max=256"`
}
