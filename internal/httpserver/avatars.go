package httpserver

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"cpa-usage-ranking/internal/avatars"
	"github.com/gin-gonic/gin"
)

// MountAvatars serves only the validated catalog and its resources, independently
// of the optional frontend and its /assets catch-all.
func MountAvatars(router *gin.Engine, catalog *avatars.Catalog, files fs.FS) error {
	type resource struct {
		data []byte
		etag string
	}
	resources := make(map[string]resource)
	paths := []string{"catalog.json"}
	for _, avatar := range catalog.List() {
		paths = append(paths, avatar.Path)
	}
	for _, path := range paths {
		data, err := fs.ReadFile(files, path)
		if err != nil {
			return fmt.Errorf("read avatar resource: %w", err)
		}
		resources[path] = resource{data: data, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(data))}
	}
	serve := func(c *gin.Context) {
		setPublicReadCORS(c)
		c.Header("Cross-Origin-Resource-Policy", "cross-origin")
		name := strings.TrimPrefix(c.Request.URL.Path, "/avatars/")
		file, ok := resources[name]
		if !ok {
			writeError(c, http.StatusNotFound, "not_found", "Not found")
			return
		}
		c.Header("Cache-Control", "public, max-age=3600, must-revalidate")
		if name == "catalog.json" {
			c.Header("Cache-Control", "no-cache")
			c.Header("Content-Type", "application/json; charset=utf-8")
		}
		c.Header("ETag", file.etag)
		// 稳定文件名不标 immutable；内容变化产生新的 ETag。
		http.ServeContent(c.Writer, c.Request, name, time.Time{}, bytes.NewReader(file.data))
	}
	options := func(c *gin.Context) {
		setPublicReadCORS(c)
		c.Header("Cross-Origin-Resource-Policy", "cross-origin")
		c.Status(http.StatusNoContent)
	}
	router.GET("/avatars/*path", serve)
	router.HEAD("/avatars/*path", serve)
	router.OPTIONS("/avatars/*path", options)
	return nil
}
