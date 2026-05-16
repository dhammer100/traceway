package services

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"strings"

	"github.com/tracewayapp/traceway/backend/app/config"

	"github.com/gorilla/sessions"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/github"
	"github.com/markbates/goth/providers/google"
)

// deriveSessionKey produces a 32-byte key from the operator-provided
// OAUTH_SESSION_SECRET, tagged so we can derive distinct authentication and
// encryption keys from one secret without reuse across primitives.
func deriveSessionKey(secret, tag string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tag))
	return mac.Sum(nil)
}

type oauthService struct {
	googleEnabled bool
	githubEnabled bool
}

var OAuthService *oauthService

func InitOAuth() {
	cfg := config.Config

	svc := &oauthService{
		googleEnabled: cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "",
		githubEnabled: cfg.GitHubClientID != "" && cfg.GitHubClientSecret != "",
	}
	OAuthService = svc

	if !svc.googleEnabled && !svc.githubEnabled {
		return
	}

	secret := cfg.OAuthSessionSecret
	if secret == "" {
		// OAuth providers are configured, so a dedicated session secret is
		// required. Sharing the JWT signing secret across two unrelated
		// primitives (one of which keys client-visible cookies) is a smell;
		// refuse to bring up rather than silently reusing it.
		panic("OAUTH_SESSION_SECRET is required when an OAuth provider is configured (must be at least 32 chars)")
	}
	if len(secret) < 32 {
		panic("OAUTH_SESSION_SECRET must be at least 32 characters")
	}

	// Use distinct keys for authentication (HMAC of cookie) and encryption
	// (AES-256 of cookie payload). The encryption key being present switches
	// the gorilla CookieStore from sign-only to encrypt-then-MAC.
	authKey := deriveSessionKey(secret, "traceway.oauth.session.auth")
	encKey := deriveSessionKey(secret, "traceway.oauth.session.enc")

	store := sessions.NewCookieStore(authKey, encKey)
	store.Options.Path = "/"
	store.Options.HttpOnly = true
	store.Options.MaxAge = 600
	store.Options.Secure = strings.HasPrefix(cfg.AppBaseURL, "https://")
	store.Options.SameSite = http.SameSiteLaxMode
	gothic.Store = store

	providers := []goth.Provider{}
	base := strings.TrimRight(cfg.AppBaseURL, "/")
	if svc.googleEnabled {
		providers = append(providers, google.New(
			cfg.GoogleClientID,
			cfg.GoogleClientSecret,
			base+"/api/auth/callback/google",
			"email", "profile",
		))
	}
	if svc.githubEnabled {
		providers = append(providers, github.New(
			cfg.GitHubClientID,
			cfg.GitHubClientSecret,
			base+"/api/auth/callback/github",
			"user:email",
		))
	}
	goth.UseProviders(providers...)
}

func (s *oauthService) IsEnabled() bool {
	return s.googleEnabled || s.githubEnabled
}

func (s *oauthService) IsProviderEnabled(name string) bool {
	switch name {
	case "google":
		return s.googleEnabled
	case "github":
		return s.githubEnabled
	}
	return false
}

func (s *oauthService) EnabledProviders() []string {
	out := []string{}
	if s.googleEnabled {
		out = append(out, "google")
	}
	if s.githubEnabled {
		out = append(out, "github")
	}
	return out
}
