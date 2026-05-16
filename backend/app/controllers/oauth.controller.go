package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/tracewayapp/traceway/backend/app/cache"
	"github.com/tracewayapp/traceway/backend/app/config"
	"github.com/tracewayapp/traceway/backend/app/middleware"
	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/repositories"
	"github.com/tracewayapp/traceway/backend/app/services"

	"github.com/gin-gonic/gin"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	traceway "go.tracewayapp.com"
)

type oauthController struct{}

type oauthProvidersResponse struct {
	Providers []string `json:"providers"`
}

type finishOAuthSetupRequest struct {
	OrganizationName string `json:"organizationName" binding:"required"`
	Timezone         string `json:"timezone" binding:"required"`
	ProjectName      string `json:"projectName" binding:"required"`
	Framework        string `json:"framework" binding:"required"`
}

func (a oauthController) ListProviders(c *gin.Context) {
	if services.OAuthService == nil {
		c.JSON(http.StatusOK, oauthProvidersResponse{Providers: []string{}})
		return
	}
	c.JSON(http.StatusOK, oauthProvidersResponse{Providers: services.OAuthService.EnabledProviders()})
}

func (a oauthController) Begin(c *gin.Context) {
	provider := c.Param("provider")
	if services.OAuthService == nil || !services.OAuthService.IsProviderEnabled(provider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown OAuth provider"})
		return
	}

	req := c.Request.WithContext(context.WithValue(c.Request.Context(), gothic.ProviderParamKey, provider))
	gothic.BeginAuthHandler(c.Writer, req)
}

func (a oauthController) Callback(c *gin.Context) {
	provider := c.Param("provider")
	if services.OAuthService == nil || !services.OAuthService.IsProviderEnabled(provider) {
		a.redirectError(c, "oauth_failed")
		return
	}

	req := c.Request.WithContext(context.WithValue(c.Request.Context(), gothic.ProviderParamKey, provider))
	gothUser, err := gothic.CompleteUserAuth(c.Writer, req)
	if err != nil {
		traceway.CaptureException(fmt.Errorf("OAuth complete failed (provider=%s): %w", provider, err))
		a.redirectError(c, "oauth_failed")
		return
	}

	if gothUser.Email == "" {
		a.redirectError(c, "oauth_no_email")
		return
	}

	user, err := a.findOrCreateUser(c, provider, gothUser)
	if err != nil {
		return
	}

	tx := middleware.GetTx(c)

	memberships, err := repositories.OrganizationRepository.FindByUserIdWithRoles(tx, user.Id)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("OAuth callback: load memberships: %w", err))
		return
	}
	needsSetup := len(memberships) == 0

	jwt, err := services.GenerateToken(user.Id, user.Email, user.TokenVersion)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("OAuth callback: generate JWT: %w", err))
		return
	}

	target := fmt.Sprintf("%s/auth/callback#token=%s&needsSetup=%t",
		strings.TrimRight(config.Config.AppBaseURL, "/"),
		url.QueryEscape(jwt),
		needsSetup,
	)
	c.Redirect(http.StatusSeeOther, target)
}

func (a oauthController) findOrCreateUser(c *gin.Context, provider string, gothUser goth.User) (*models.User, error) {
	tracewayTx := middleware.GetTx(c)

	user, err := repositories.UserRepository.FindByOAuth(tracewayTx, provider, gothUser.UserID)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("OAuth callback: lookup by provider: %w", err))
		return nil, err
	}
	if user != nil {
		return user, nil
	}

	existing, err := repositories.UserRepository.FindByEmailIgnoreCase(tracewayTx, gothUser.Email)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("OAuth callback: lookup by email: %w", err))
		return nil, err
	}

	if existing != nil {
		// An account already exists with this email. Auto-linking on email
		// match alone is an account-takeover vector when the provider returns
		// an unverified email — the safer path is to make the user prove
		// control of the existing account (log in via password or the
		// already-linked provider) before we associate this OAuth identity.
		// Linking from the settings UI is out of scope for this change.
		a.redirectError(c, "email_in_use_signin_with_existing_method")
		return nil, errors.New("email already associated with another account")
	}

	if config.Config.CloudMode != "true" {
		hasOrg, err := repositories.OrganizationRepository.HasOrganizations(tracewayTx)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("OAuth callback: count orgs: %w", err))
			return nil, err
		}
		if hasOrg {
			a.redirectError(c, "invite_required")
			return nil, errors.New("invite required")
		}
	}

	name := gothUser.Name
	if name == "" {
		name = gothUser.NickName
	}
	if name == "" {
		name = gothUser.Email
	}

	created, err := repositories.UserRepository.CreateOAuth(tracewayTx, gothUser.Email, name, provider, gothUser.UserID, gothUser.AvatarURL)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("OAuth callback: create user: %w", err))
		return nil, err
	}
	return created, nil
}

func (a oauthController) FinishSetup(c *gin.Context) {
	var request finishOAuthSetupRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if !validFrameworks[request.Framework] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Framework must be one of: gin, fiber, chi, fasthttp, stdlib, custom, react, svelte, vuejs, jquery, react-native, hono, cloudflare, opentelemetry, symfony, flutter, android"})
		return
	}

	userId := middleware.GetUserId(c)
	if userId == 0 {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	tx := middleware.GetTx(c)

	user, err := repositories.UserRepository.FindById(tx, userId)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: load user: %w", err))
		return
	}
	if user == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	memberships, err := repositories.OrganizationRepository.FindByUserIdWithRoles(tx, user.Id)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: load memberships: %w", err))
		return
	}
	if len(memberships) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Setup already complete"})
		return
	}

	if config.Config.CloudMode != "true" {
		hasOrg, err := repositories.OrganizationRepository.HasOrganizations(tx)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: count orgs: %w", err))
			return
		}
		if hasOrg {
			c.JSON(http.StatusConflict, gin.H{"error": "An organization already exists. Please ask an admin to invite you."})
			return
		}
	}

	org, err := repositories.OrganizationRepository.Create(tx, request.OrganizationName, request.Timezone)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: create org: %w", err))
		return
	}

	if _, err := repositories.OrganizationRepository.AddUser(tx, org.Id, user.Id, "owner"); err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: add user to org: %w", err))
		return
	}

	for _, hook := range PostRegistrationHooks {
		if err := hook(tx, org, user); err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: post-registration hook: %w", err))
			return
		}
	}

	project, err := repositories.ProjectRepository.CreateWithOrganization(tx, request.ProjectName, request.Framework, org.Id)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: create project: %w", err))
		return
	}

	cachedProject := &models.Project{
		Id:             project.Id,
		Name:           project.Name,
		Token:          project.Token,
		Framework:      project.Framework,
		OrganizationId: project.OrganizationId,
	}
	middleware.AfterCommit(c, func() {
		cache.ProjectCache.AddProject(cachedProject)
	})

	token, err := services.GenerateToken(user.Id, user.Email, user.TokenVersion)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: regenerate token: %w", err))
		return
	}

	projects, err := repositories.ProjectRepository.FindAllWithBackendUrlByUserId(tx, user.Id)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: load projects: %w", err))
		return
	}

	organizations, err := repositories.OrganizationRepository.FindByUserIdWithRoles(tx, user.Id)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("FinishSetup: load orgs: %w", err))
		return
	}

	c.JSON(http.StatusCreated, &models.RegisterResponse{
		Token:         token,
		User:          user.ToResponse(),
		Project:       *project.ToProjectWithBackendUrl(),
		Projects:      projects,
		Organizations: organizations,
	})
}

func (a oauthController) redirectError(c *gin.Context, code string) {
	target := fmt.Sprintf("%s/login?error=%s", strings.TrimRight(config.Config.AppBaseURL, "/"), url.QueryEscape(code))
	c.Redirect(http.StatusSeeOther, target)
}

var OAuthController = oauthController{}
