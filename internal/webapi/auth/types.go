package auth

type signUpRequest struct {
	// Username username, must be unique
	Username string `json:"username" validate:"required"`
	// Password user password, min 8 characters
	Password string `json:"password" validate:"required,min=8"`
}

type logInRequest struct {
	// Username
	Username string `json:"username" validate:"required"`
	// Password user password
	Password string `json:"password" validate:"required"`
}

type authResponse struct {
	// ID is user id (uuid)
	ID string `json:"id"`
	// Token user auth token (Bearer)
	Token string `json:"token"`
	// RefreshToken user refresh token
	RefreshToken string `json:"refresh_token"`
}

type refreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}
