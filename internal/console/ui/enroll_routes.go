package ui

import (
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// The enrollment and downloads routes (architecture §5.3 /enroll, /downloads;
// ticket M2-W2B). They are registered from one place so ui.go keeps a single
// additive line, and listed in enrollUIPatterns so the route-coverage tests see
// them. Every one needs the admin role (§5.3 "who" AD); every POST is
// CSRF-checked by Require.
const (
	patEnroll        = "GET /enroll"
	patEnrollCreate  = "POST /ui/enroll/token"
	patEnrollToken   = "GET /enroll/tokens/{id}"
	patEnrollRevoke  = "POST /ui/enroll/tokens/{id}/revoke"
	patDownloadsPage = "GET /downloads"
)

func enrollUIPatterns() []string {
	return []string{patEnroll, patEnrollCreate, patEnrollToken, patEnrollRevoke, patDownloadsPage}
}

// EnrollUIRoles is the minimum role of each pattern, for the role-gating test.
func EnrollUIRoles() map[string]string {
	return map[string]string{patEnroll: "AD", patEnrollCreate: "AD", patEnrollToken: "AD", patEnrollRevoke: "AD", patDownloadsPage: "AD"}
}

func (s *Service) registerEnroll(r httpx.Registrar) {
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	r.Handle(patEnroll, ad(s.enrollPage))
	r.Handle(patEnrollCreate, ad(s.enrollCreate))
	r.Handle(patEnrollToken, ad(s.enrollTokenPage))
	r.Handle(patEnrollRevoke, ad(s.enrollRevoke))
	r.Handle(patDownloadsPage, ad(s.downloadsPage))
}
