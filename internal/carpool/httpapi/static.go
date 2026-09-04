package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	carpoolweb "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/web"
)

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// RegisterStaticRoutes attaches the embedded carpool console without a SPA catch-all.
func RegisterStaticRoutes(engine *gin.Engine) {
	if engine == nil {
		return
	}
	engine.GET("/carpool", func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, "/carpool/")
	})
	engine.GET("/carpool/", serveEmbeddedAsset("index.html", false))
	engine.GET("/carpool/assets/*filepath", func(c *gin.Context) {
		name := strings.TrimPrefix(c.Param("filepath"), "/")
		if name == "" {
			c.Status(http.StatusNotFound)
			return
		}
		serveEmbeddedAsset(name, true)(c)
	})
}

func serveEmbeddedAsset(name string, immutable bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		asset, errRead := carpoolweb.Read(name)
		if errRead != nil {
			c.Status(http.StatusNotFound)
			return
		}
		setStaticSecurityHeaders(c)
		c.Header("ETag", asset.ETag)
		if immutable {
			c.Header("Cache-Control", "public, max-age=300, must-revalidate")
		} else {
			c.Header("Cache-Control", "no-cache")
		}
		if c.GetHeader("If-None-Match") == asset.ETag {
			c.Status(http.StatusNotModified)
			return
		}
		c.Data(http.StatusOK, asset.ContentType, asset.Body)
	}
}

func setStaticSecurityHeaders(c *gin.Context) {
	c.Header("Content-Security-Policy", contentSecurityPolicy)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}
