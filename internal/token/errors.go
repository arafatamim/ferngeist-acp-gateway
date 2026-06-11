package token

import "errors"

var (
	ErrTokenInvalid = errors.New("attach token is invalid or expired")
	ErrTokenExpired = errors.New("attach token has expired")
)
