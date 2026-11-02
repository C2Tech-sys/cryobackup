package adminapi

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The enrollment-token routes of architecture §5.2 (M2-W2B): list, create, get,
// revoke and the deployment-script generator. Every one needs the admin role.
// The secret of a token is returned once, in the body of the create call, and is
// never stored, logged or audited (only its SHA-256 is kept).
const (
	PatternEnrollTokens      = "GET /api/v1/enrollment-tokens"
	PatternEnrollTokenCreate = "POST /api/v1/enrollment-tokens"
	PatternEnrollToken       = "GET /api/v1/enrollment-tokens/{id}"
	PatternEnrollTokenRevoke = "DELETE /api/v1/enrollment-tokens/{id}"
	PatternEnrollTokenScript = "GET /api/v1/enrollment-tokens/{id}/script"

	// EnrollTokenHeader carries the secret into the script route. A header, not
	// a query parameter: query strings end up in proxy logs and browser history.
	EnrollTokenHeader = "X-Enrollment-Token"
)

// EnrollPatterns lists the enrollment routes with the role each needs.
func EnrollPatterns() map[string]string {
	return map[string]string{PatternEnrollTokens: "AD", PatternEnrollTokenCreate: "AD", PatternEnrollToken: "AD",
		PatternEnrollTokenRevoke: "AD", PatternEnrollTokenScript: "AD"}
}

func enrollPatternList() []string {
	return []string{PatternEnrollTokens, PatternEnrollTokenCreate, PatternEnrollToken, PatternEnrollTokenRevoke, PatternEnrollTokenScript}
}

func (s *Service) registerEnroll(r httpx.Registrar) {
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, h) }
	r.Handle(PatternEnrollTokens, ad(s.listEnrollTokens))
	r.Handle(PatternEnrollTokenCreate, ad(s.createEnrollToken))
	r.Handle(PatternEnrollToken, ad(s.getEnrollToken))
	r.Handle(PatternEnrollTokenRevoke, ad(s.revokeEnrollToken))
	r.Handle(PatternEnrollTokenScript, ad(s.enrollTokenScript))
}

// EnrollTokenJSON is a token as the API shows it: never the secret, never its hash.
type EnrollTokenJSON struct {
	ID          string     `json:"id"`
	CustomerID  string     `json:"customer_id"`
	Customer    string     `json:"customer"`
	Label       string     `json:"label,omitempty"`
	PolicyID    string     `json:"policy_id,omitempty"`
	PolicyName  string     `json:"policy_name,omitempty"`
	DeviceClass string     `json:"device_class"`
	Status      string     `json:"status"`
	ExpiresAt   time.Time  `json:"expires_at"`
	MaxUses     *int       `json:"max_uses"`
	UsedCount   int        `json:"used_count"`
	UsesLeft    *int       `json:"uses_left"`
	Devices     int        `json:"devices"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// EnrollTokenListResponse is GET /api/v1/enrollment-tokens.
type EnrollTokenListResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	// Total counts the tokens that match, Of every token in the caller's scope.
	Total      int               `json:"total"`
	Of         int               `json:"of"`
	Truncated  bool              `json:"truncated"`
	NextCursor string            `json:"next_cursor,omitempty"`
	Tokens     []EnrollTokenJSON `json:"tokens"`
}

// EnrollTokenResponse is GET /api/v1/enrollment-tokens/{id}.
type EnrollTokenResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	EnrollTokenJSON
}

// CreateEnrollTokenRequest is the body of POST /api/v1/enrollment-tokens.
type CreateEnrollTokenRequest struct {
	CustomerID string `json:"customer_id"`
	Label      string `json:"label"`
	PolicyID   string `json:"policy_id"`
	// DeviceClass is the class a device gets when its agent does not say: laptop, desktop or unknown.
	DeviceClass string `json:"device_class"`
	// ExpiresIn is a duration such as "12h", "7d" or "2w" (default 7d, at most 90d).
	ExpiresIn string `json:"expires_in"`
	// MaxUses is the most enrollments the token allows (absent or null: unlimited).
	MaxUses *int `json:"max_uses"`
}

// CreateEnrollTokenResponse is the answer of the create call: the token, with
// its secret, once. The secret is in no other response and cannot be read back.
type CreateEnrollTokenResponse struct {
	EnrollTokenResponse
	Token string `json:"token"`
}

// DefaultTokenLifetime is the expiry of a token created without one.
const DefaultTokenLifetime = 7 * 24 * time.Hour

func enrollTokenJSON(r store.EnrollTokenRow, now time.Time) EnrollTokenJSON {
	out := EnrollTokenJSON{ID: r.ID, CustomerID: r.CustomerID, Customer: r.CustomerName, Label: r.Label, PolicyID: r.PolicyID, PolicyName: r.PolicyName,
		DeviceClass: r.DeviceClassDefault, Status: r.Status(now), ExpiresAt: r.ExpiresAt.UTC(), MaxUses: r.MaxUses, UsedCount: r.UsedCount,
		Devices: r.Devices, CreatedBy: r.CreatedByName, CreatedAt: r.CreatedAt.UTC()}
	if left := r.UsesLeft(); left >= 0 {
		out.UsesLeft = &left
	}
	if r.LastUsedAt != nil {
		out.LastUsedAt = ptrTime(*r.LastUsedAt)
	}
	if r.RevokedAt != nil {
		out.RevokedAt = ptrTime(*r.RevokedAt)
	}
	return out
}

func (s *Service) listEnrollTokens(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 500)
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	l, err := s.Store.ListEnrollmentTokenRows(r.Context(), p.Scope, store.EnrollTokenFilter{CustomerID: q.Get("customer"), Status: q.Get("status"), Offset: offset, Limit: limit})
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	now := s.Store.Now()
	resp := EnrollTokenListResponse{ComputedAt: now, Total: l.Page.Total, Of: l.All, Tokens: make([]EnrollTokenJSON, 0, len(l.Rows))}
	resp.NextCursor, resp.Truncated = nextCursor(l.Page)
	for _, row := range l.Rows {
		resp.Tokens = append(resp.Tokens, enrollTokenJSON(row, now))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) getEnrollToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	row, err := s.Store.GetEnrollmentTokenRow(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	now := s.Store.Now()
	httpx.WriteJSON(w, http.StatusOK, EnrollTokenResponse{ComputedAt: now, EnrollTokenJSON: enrollTokenJSON(row, now)})
}

func (s *Service) createEnrollToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreateEnrollTokenRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	ttl := DefaultTokenLifetime
	if req.ExpiresIn != "" {
		d, err := enroll.ParseDuration(req.ExpiresIn)
		if err != nil {
			s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: "expires_in", Message: "Use a duration such as 12h, 7d or 2w."}}})
			return
		}
		ttl = d
	}
	plain, hash, err := enroll.NewToken()
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	tok, err := s.Store.CreateEnrollmentTokenAudited(r.Context(), p.Scope, store.EnrollTokenInput{CustomerID: req.CustomerID, Label: req.Label, PolicyID: req.PolicyID,
		DeviceClass: req.DeviceClass, ExpiresAt: s.Store.Now().Add(ttl), MaxUses: req.MaxUses, TokenHash: hash}, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	row, err := s.Store.GetEnrollmentTokenRow(r.Context(), p.Scope, tok.ID)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	now := s.Store.Now()
	w.Header().Set("Location", "/api/v1/enrollment-tokens/"+tok.ID)
	httpx.WriteJSON(w, http.StatusCreated, CreateEnrollTokenResponse{EnrollTokenResponse: EnrollTokenResponse{ComputedAt: now, EnrollTokenJSON: enrollTokenJSON(row, now)}, Token: plain})
}

func (s *Service) revokeEnrollToken(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if _, err := s.Store.RevokeEnrollmentTokenAudited(r.Context(), p.Scope, r.PathValue("id"), actorOf(r, p)); err != nil {
		s.failM2(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// enrollTokenScript answers GET .../script?format=ps1|msi|gpo with the snippet
// as plain text. The caller presents the secret it was shown at creation in the
// X-Enrollment-Token header; the console checks it against the stored hash (it
// cannot regenerate a secret) and refuses a token that can no longer enroll.
func (s *Service) enrollTokenScript(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	format := r.URL.Query().Get("format")
	if format != "ps1" && format != "msi" && format != "gpo" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	secret := r.Header.Get(EnrollTokenHeader)
	row, err := s.Store.GetEnrollmentTokenRow(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if !enroll.WellFormed(secret) || subtle.ConstantTimeCompare([]byte(enroll.HashToken(secret)), []byte(row.TokenHash)) != 1 {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	if row.Status(s.Store.Now()) != store.TokenActive {
		httpx.Problem(w, r, api.CodeConflict, 0)
		return
	}
	msi, merr := CurrentMSI(s.ArtifactsDir)
	if merr != nil {
		msi = MSIInfo{}
	}
	sn := GenerateSnippets(SnippetInput{PublicURL: s.PublicURL, Token: secret, MSI: msi})
	text := map[string]string{"ps1": sn.RMM, "msi": sn.MSIExec + "\n", "gpo": sn.GPO}[format]
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}
