package web

import (
	"net/http"

	"github.com/abbot/go-http-auth"
	"github.com/shellster/eufy_cam/config"
)

func NewDigestMiddleware(authCfg *config.AuthConfig) func(http.Handler) http.Handler {
	if authCfg == nil || !authCfg.IsDigest() {
		return func(next http.Handler) http.Handler { return next }
	}

	a := auth.NewDigestAuthenticator("eufy_cam", func(user, realm string) string {
		if user == authCfg.Username {
			return authCfg.Password
		}
		return ""
	})
	a.PlainTextSecrets = true

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if username, _ := a.CheckAuth(r); username != "" {
				next.ServeHTTP(w, r)
				return
			}
			a.RequireAuth(w, r)
		})
	}
}
