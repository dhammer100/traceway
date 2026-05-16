package services

import (
	"github.com/tracewayapp/traceway/backend/app/config"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var jwtSecret []byte

type JWTClaims struct {
	UserId       int    `json:"userId"`
	Email        string `json:"email"`
	TokenVersion int    `json:"tv"`
	jwt.RegisteredClaims
}

func InitJWT() error {
	secret := config.Config.JWTSecret
	if secret == "" {
		return errors.New("JWT_SECRET environment variable is not set")
	}
	if len(secret) < 32 {
		return errors.New("JWT_SECRET must be at least 32 characters")
	}
	jwtSecret = []byte(secret)
	return nil
}

func GenerateToken(userId int, email string, tokenVersion int) (string, error) {
	claims := JWTClaims{
		UserId:       userId,
		Email:        email,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			// 24h matches the dashboard's typical session window. The
			// tokenVersion check still kicks in on password change to cut this
			// short, but a shorter TTL also bounds the window for stolen
			// tokens (e.g. via XSS-pulled localStorage).
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

func ValidateToken(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*JWTClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}
