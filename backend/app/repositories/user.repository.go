package repositories

import (
	"database/sql"
	"strings"
	"time"

	"github.com/tracewayapp/traceway/backend/app/db"
	"github.com/tracewayapp/traceway/backend/app/models"

	"github.com/tracewayapp/lit/v2"
)

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

type userRepository struct{}

const userColumns = "id, email, name, password, created_at, oauth_provider, oauth_user_id, avatar_url, token_version"

// FindByEmail looks up a user by email, case-insensitively. Email is treated
// as a case-insensitive identifier everywhere — register/login/forgot-password
// all funnel through this. The pre-existing FindByEmailIgnoreCase is kept as a
// thin alias for callers that document intent explicitly.
func (r *userRepository) FindByEmail(tx *sql.Tx, email string) (*models.User, error) {
	return lit.SelectSingleNamed[models.User](
		tx,
		"SELECT "+userColumns+" FROM users WHERE LOWER(email) = LOWER(:email)",
		lit.P{"email": email},
	)
}

func (r *userRepository) FindByEmailIgnoreCase(tx *sql.Tx, email string) (*models.User, error) {
	return r.FindByEmail(tx, email)
}

func (r *userRepository) FindById(tx *sql.Tx, id int) (*models.User, error) {
	return lit.SelectSingleNamed[models.User](
		tx,
		"SELECT "+userColumns+" FROM users WHERE id = :id",
		lit.P{"id": id},
	)
}

func (r *userRepository) FindByOAuth(tx *sql.Tx, provider string, providerUserId string) (*models.User, error) {
	return lit.SelectSingleNamed[models.User](
		tx,
		"SELECT "+userColumns+" FROM users WHERE oauth_provider = :provider AND oauth_user_id = :uid",
		lit.P{"provider": provider, "uid": providerUserId},
	)
}

func (r *userRepository) Create(tx *sql.Tx, email string, name string, hashedPassword string) (*models.User, error) {
	user := &models.User{
		Email:     normalizeEmail(email),
		Name:      name,
		Password:  hashedPassword,
		CreatedAt: time.Now().UTC(),
	}

	id, err := lit.Insert(tx, user)
	if err != nil {
		return nil, err
	}
	user.Id = id

	return user, nil
}

func (r *userRepository) CreateOAuth(tx *sql.Tx, email, name, provider, providerUserId, avatarUrl string) (*models.User, error) {
	var avatar *string
	if avatarUrl != "" {
		avatar = &avatarUrl
	}
	user := &models.User{
		Email:         normalizeEmail(email),
		Name:          name,
		Password:      "",
		CreatedAt:     time.Now().UTC(),
		OauthProvider: &provider,
		OauthUserId:   &providerUserId,
		AvatarUrl:     avatar,
	}

	id, err := lit.Insert(tx, user)
	if err != nil {
		return nil, err
	}
	user.Id = id

	return user, nil
}

func (r *userRepository) LinkOAuth(tx *sql.Tx, userId int, provider, providerUserId, avatarUrl string) error {
	q, a, err := lit.ParseNamedQuery(
		db.Driver,
		"UPDATE users SET oauth_provider = :provider, oauth_user_id = :uid, avatar_url = COALESCE(:avatar, avatar_url) WHERE id = :id",
		lit.P{
			"provider": provider,
			"uid":      providerUserId,
			"avatar":   nullIfEmpty(avatarUrl),
			"id":       userId,
		},
	)
	if err != nil {
		return err
	}
	return lit.UpdateNative(tx, q, a...)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (r *userRepository) EmailExists(tx *sql.Tx, email string) (bool, error) {
	user, err := r.FindByEmail(tx, email)
	if err != nil {
		return false, err
	}
	return user != nil, nil
}

func (r *userRepository) SetPasswordResetToken(tx *sql.Tx, userId int, token string, expiresAt time.Time) error {
	now := time.Now()
	return lit.UpdateNamed[models.User](
		tx,
		&models.User{
			PasswordResetToken:       &token,
			PasswordResetExpiresAt:   &expiresAt,
			PasswordResetRequestedAt: &now,
		},
		"id = :id",
		lit.P{"id": userId},
	)
}

func (r *userRepository) ClearPasswordResetToken(tx *sql.Tx, userId int) error {
	q, a, err := lit.ParseNamedQuery(db.Driver, "UPDATE users SET password_reset_token = NULL, password_reset_expires_at = NULL, password_reset_requested_at = NULL WHERE id = :id", lit.P{"id": userId})
	if err != nil {
		return err
	}
	_, err = tx.Exec(q, a...)
	return err
}

func (r *userRepository) FindByPasswordResetToken(tx *sql.Tx, token string) (*models.User, error) {
	return lit.SelectSingleNamed[models.User](
		tx,
		"SELECT "+userColumns+", password_reset_token, password_reset_expires_at, password_reset_requested_at FROM users WHERE password_reset_token = :token",
		lit.P{"token": token},
	)
}

// UpdatePassword updates the user's password and bumps token_version so that
// any JWT issued before the reset is invalidated. Uses raw UPDATE to avoid
// lit.UpdateNamed clobbering other columns with their zero values.
func (r *userRepository) UpdatePassword(tx *sql.Tx, userId int, hashedPassword string) error {
	q, a, err := lit.ParseNamedQuery(
		db.Driver,
		"UPDATE users SET password = :password, token_version = token_version + 1 WHERE id = :id",
		lit.P{"password": hashedPassword, "id": userId},
	)
	if err != nil {
		return err
	}
	return lit.UpdateNative(tx, q, a...)
}

// GetTokenVersionByIdNoTx looks up token_version without a surrounding
// transaction. UseAppAuth runs before Transactional, so it can't share a tx
// with the rest of the request — instead it goes straight at the pool. The
// column is indexed (primary key on id) so the cost is negligible.
func (r *userRepository) GetTokenVersionByIdNoTx(userId int) (int, bool, error) {
	q, a, err := lit.ParseNamedQuery(db.Driver, "SELECT token_version FROM users WHERE id = :id", lit.P{"id": userId})
	if err != nil {
		return 0, false, err
	}
	row := db.DB.QueryRow(q, a...)
	var v int
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	return v, true, nil
}

var UserRepository = userRepository{}
