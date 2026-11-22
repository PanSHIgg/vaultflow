package api

import (
	"net/http"

	"vaultflow/gateway-core/internal/auth"
	"vaultflow/gateway-core/internal/store"
)

// ---------------------------------------------------------------------------
// POST /v1/auth/register, /v1/auth/login — 用户认证 (public, IP 限流)
// ---------------------------------------------------------------------------

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// authResponse {user_id, token} — web AuthResponse 契约.
type authResponse struct {
	UserID string `json:"user_id"`
	Token  string `json:"token"`
}

// register 注册: 201 {user_id, token}; 400 INVALID_INPUT; 409 EMAIL_TAKEN.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	u, err := s.deps.Exchange.RegisterUser(r.Context(), req.Email, req.Password)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	s.issueUserSession(w, r, u, http.StatusCreated)
}

// login 登录: 200 {user_id, token}; 401 BAD_CREDENTIALS (邮箱不存在与口令
// 错误统一返回, 不泄露账号存在性).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
		writeError(w, r, ae)
		return
	}
	u, err := s.deps.Exchange.AuthenticateUser(r.Context(), req.Email, req.Password)
	if err != nil {
		writeError(w, r, mapError(err))
		return
	}
	s.issueUserSession(w, r, u, http.StatusOK)
}

// issueUserSession 签发 USER JWT 并输出 {user_id, token}.
func (s *Server) issueUserSession(w http.ResponseWriter, r *http.Request, u store.User, status int) {
	if len(s.deps.UserJWTKey) == 0 {
		writeError(w, r, internalErr("user auth not configured"))
		return
	}
	tok, err := auth.IssueUserToken(s.deps.UserJWTKey, u.ID, auth.DefaultTokenTTL)
	if err != nil {
		writeError(w, r, internalErr("token issuance failed"))
		return
	}
	writeJSON(w, status, authResponse{UserID: u.ID.String(), Token: tok})
}
