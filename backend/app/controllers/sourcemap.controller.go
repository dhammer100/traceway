package controllers

import (
	"github.com/tracewayapp/traceway/backend/app/middleware"
	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/db"
	"github.com/tracewayapp/traceway/backend/app/repositories"
	"github.com/tracewayapp/traceway/backend/app/storage"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	traceway "go.tracewayapp.com"
)

// safeKeyComponent is the alphabet allowed for the parts of a storage key that
// originate from client input (version, filename). Anything else is rejected
// rather than silently rewritten, so an upload that would have written outside
// the storage root fails closed.
var safeKeyComponent = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func sanitizeKeyComponent(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if strings.ContainsAny(s, "\x00/\\") || s == "." || s == ".." {
		return "", false
	}
	if !safeKeyComponent.MatchString(s) {
		return "", false
	}
	return s, true
}

type sourceMapController struct{}

func (s sourceMapController) Upload(c *gin.Context) {
	projectId, err := middleware.GetProjectId(c)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("UseSourceMapAuth middleware must be applied: %w", err))
		return
	}

	if err := c.Request.ParseMultipartForm(50 << 20); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse multipart form"})
		return
	}

	rawVersion := c.Request.FormValue("version")
	if rawVersion == "" {
		rawVersion = "unversioned"
	}
	version, ok := sanitizeKeyComponent(rawVersion)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version (use [A-Za-z0-9._-], max 128 chars)"})
		return
	}

	files := c.Request.MultipartForm.File["files"]
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No files uploaded"})
		return
	}

	uploaded := 0
	for _, fileHeader := range files {
		// Always reduce the client-supplied filename to its basename before any
		// further use — multipart filenames are attacker-controlled.
		baseName := filepath.Base(fileHeader.Filename)
		if !strings.HasSuffix(baseName, ".map") {
			continue
		}

		safeFileName, ok := sanitizeKeyComponent(baseName)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid filename %q (use [A-Za-z0-9._-], max 128 chars)", baseName)})
			return
		}

		if fileHeader.Size > 50<<20 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("File %s exceeds 50MB limit", safeFileName)})
			return
		}

		f, err := fileHeader.Open()
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("failed to open uploaded file %s: %w", safeFileName, err))
			return
		}

		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("failed to read uploaded file %s: %w", safeFileName, err))
			return
		}

		storageKey := fmt.Sprintf("sourcemaps/%s/%s/%s", projectId, version, safeFileName)

		if err := storage.Store.Write(c, storageKey, data); err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("failed to write source map to storage: %w", err))
			return
		}

		_, err = db.ExecuteTransaction(func(tx *sql.Tx) (*models.SourceMap, error) {
			existing, err := repositories.SourceMapRepository.FindByProjectVersionAndFileName(tx, projectId, version, safeFileName)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				existing.StorageKey = storageKey
				existing.FileSize = fileHeader.Size
				existing.UploadedAt = time.Now().UTC()
				return existing, repositories.SourceMapRepository.Update(tx, existing)
			}
			return repositories.SourceMapRepository.Create(tx, &models.SourceMap{
				ProjectId:  projectId,
				Version:    version,
				FileName:   safeFileName,
				StorageKey: storageKey,
				FileSize:   fileHeader.Size,
			})
		})
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("failed to upsert source map metadata: %w", err))
			return
		}

		uploaded++
	}

	c.JSON(http.StatusOK, gin.H{"uploaded": uploaded})
}

var SourceMapController = sourceMapController{}
