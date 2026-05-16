package models

import (
	"time"
)

type User struct {
	Id                       int        `json:"id"`
	Email                    string     `json:"email"`
	Name                     string     `json:"name"`
	Password                 string     `json:"-"`
	CreatedAt                time.Time  `json:"createdAt"`
	PasswordResetToken       *string    `json:"-"`
	PasswordResetExpiresAt   *time.Time `json:"-"`
	PasswordResetRequestedAt *time.Time `json:"-"`
	OauthProvider            *string    `json:"-"`
	OauthUserId              *string    `json:"-"`
	AvatarUrl                *string    `json:"avatarUrl,omitempty"`
	// TokenVersion is bumped on password change so previously-issued JWTs are
	// rejected. UseAppAuth verifies the claim against the live column.
	TokenVersion int `json:"-" lit:"token_version"`
}

type UserResponse struct {
	Id        int       `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

func (u *User) ToResponse() UserResponse {
	return UserResponse{
		Id:        u.Id,
		Email:     u.Email,
		Name:      u.Name,
		CreatedAt: u.CreatedAt,
	}
}
