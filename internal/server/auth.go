package server

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

import (
	qrcode "github.com/skip2/go-qrcode"
)

import (
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/web"
)

// This file is the gateway's own authentication surface: session cookies and
// two-factor login for the gateway itself (the proxied backends authenticate
// separately, through the identity headers the proxy injects).

// currentSessionRev returns the per-account session revision derived from the
// user's current password hash. Because the revision is embedded in session
// tokens, changing the password revokes all previously-issued sessions.
func (s *Server) currentSessionRev(username string) string {
	if !config.IsValidUsername(username) {
		return ""
	}
	uc := s.registry.GetUserConfigForLogin(username)
	if uc == nil {
		return ""
	}
	return auth.SessionRev(uc.Auth.PasswordHash)
}

// issueSessionCookie signs a session cookie bound to the user's current
// password-hash revision.
func (s *Server) issueSessionCookie(username string, secure bool) string {
	return auth.SetSessionCookie(s.sessionSecret, s.state.Auth.SessionTTL, username, s.currentSessionRev(username), secure)
}

// sessionFromCookies validates the session cookie, binding it to the user's
// current password-hash revision.
func (s *Server) sessionFromCookies(cookieHeader string) auth.SessionResult {
	cookies := auth.ParseCookies(cookieHeader)
	token := cookies[auth.SessionCookieName]
	if token == "" {
		return auth.SessionResult{}
	}
	userID, _, _, err := auth.SessionTokenParts(token)
	if err != nil || userID == "" {
		return auth.SessionResult{}
	}
	return auth.ValidateSessionToken(token, s.sessionSecret, s.currentSessionRev(userID))
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !validLoginNext(next) {
		next = ""
	}
	sendHTML(w, 200, web.LoginPage(s.siteTitle, "", next))
}

// validLoginNext restricts post-login redirects to in-site proxy URLs,
// preventing open-redirect abuse.
func validLoginNext(next string) bool {
	return strings.HasPrefix(next, "/proxy/") && !strings.HasPrefix(next, "//")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Parse the form before rate limiting: the limit key combines the client
	// IP with the submitted username so a shared-NAT office/lab does not lock
	// out everyone on one user's typos, and an attacker cannot freeze a whole
	// subnet by flooding failures.
	if err := r.ParseForm(); err != nil {
		sendHTML(w, 500, web.LoginPage(s.siteTitle, "登录请求处理失败", ""))
		return
	}

	ip := auth.ClientIP(r)
	username := strings.TrimSpace(r.FormValue("username"))
	// Username matching is case-sensitive, but rate limiting deliberately is
	// not, so "Alice" and "alice" share a failure budget.
	rateKey := ip + "|" + strings.ToLower(username)

	if s.loginLimiter.IsBlocked(rateKey) {
		sendHTML(w, 429, web.LoginPage(s.siteTitle, "登录尝试过多，请 15 分钟后再试", ""))
		return
	}

	password := r.FormValue("password")
	next := r.FormValue("next")
	if !validLoginNext(next) {
		next = ""
	}

	if !config.IsValidUsername(username) || password == "" {
		s.loginLimiter.RecordFailure(rateKey)
		sendHTML(w, 401, web.LoginPage(s.siteTitle, loginErrorMsg, next))
		return
	}

	userConfig := s.registry.GetUserConfigForLogin(username)
	passwordHash := ""
	if userConfig != nil {
		passwordHash = userConfig.Auth.PasswordHash
	}
	// Timing-safe verify: even for unknown usernames a bcrypt comparison runs,
	// so login latency cannot be used to enumerate valid accounts.
	if auth.VerifyPasswordTimingSafe(password, passwordHash) {
		s.loginLimiter.Reset(rateKey)
		secure := auth.IsSecureRequest(r)
		if next == "" {
			next = "/"
		}
		// Account has TOTP enabled: hold at the second factor before issuing
		// a real session.
		if userConfig != nil && userConfig.Auth.TOTPSecret != "" {
			w.Header().Set("Set-Cookie", auth.SetPendingCookie(s.sessionSecret, pendingTTLSeconds, username, secure))
			http.Redirect(w, r, "/login/2fa?next="+url.QueryEscape(next), http.StatusFound)
			return
		}
		cookie := s.issueSessionCookie(username, secure)
		w.Header().Set("Set-Cookie", cookie)
		http.Redirect(w, r, next, http.StatusFound)
		return
	}

	s.loginLimiter.RecordFailure(rateKey)
	sendHTML(w, 401, web.LoginPage(s.siteTitle, loginErrorMsg, next))
}

// ---- two-factor authentication (TOTP) ----

// handleTwoFA serves the second login step: GET renders the code form (only
// when a valid pending token exists); POST verifies the code.
func (s *Server) handleTwoFA(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		if !auth.SameOriginRequest(r) {
			sendHTML(w, 403, web.TwoFAPage(s.siteTitle, "请求被拒绝", ""))
			return
		}
		s.handleTwoFAPost(w, r)
		return
	}

	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	userID, ok := auth.ValidatePendingToken(cookies[auth.TOTACookieName], s.sessionSecret)
	if !ok || userID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	next := r.URL.Query().Get("next")
	if !validLoginNext(next) {
		next = ""
	}
	sendHTML(w, 200, web.TwoFAPage(s.siteTitle, "", next))
}

func (s *Server) handleTwoFAPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		sendHTML(w, 500, web.TwoFAPage(s.siteTitle, "请求处理失败", ""))
		return
	}

	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	userID, ok := auth.ValidatePendingToken(cookies[auth.TOTACookieName], s.sessionSecret)
	if !ok || userID == "" {
		// Pending token missing/expired: back to the password step.
		next := r.FormValue("next")
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}

	ip := auth.ClientIP(r)
	rateKey := ip + "|2fa|" + strings.ToLower(userID)
	if s.totpLimiter.IsBlocked(rateKey) {
		sendHTML(w, 429, web.TwoFAPage(s.siteTitle, "验证尝试过多，请 15 分钟后再试", r.FormValue("next")))
		return
	}

	uc := s.registry.GetUserConfigForLogin(userID)
	if uc == nil || uc.Auth.TOTPSecret == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.VerifyTOTP(uc.Auth.TOTPSecret, code, time.Now()) {
		s.totpLimiter.RecordFailure(rateKey)
		sendHTML(w, 401, web.TwoFAPage(s.siteTitle, "动态码错误或已过期", r.FormValue("next")))
		return
	}

	s.totpLimiter.Reset(rateKey)
	secure := auth.IsSecureRequest(r)
	w.Header().Set("Set-Cookie", s.issueSessionCookie(userID, secure))
	w.Header().Add("Set-Cookie", auth.ClearPendingCookie())
	next := r.FormValue("next")
	if !validLoginNext(next) {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// handleTwoFASetup enrolls (or rotates) TOTP for the logged-in user.
func (s *Server) handleTwoFASetup(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.Method == "POST" {
		if !auth.SameOriginRequest(r) {
			sendHTML(w, 403, web.TwoFASetupPage(s.siteTitle, "请求被拒绝", "", s.twoFAEnabled(session.UserID)))
			return
		}
		s.handleTwoFASetupPost(w, r, session.UserID)
		return
	}

	enabled := s.twoFAEnabled(session.UserID)
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		sendHTML(w, 500, web.TwoFASetupPage(s.siteTitle, "生成密钥失败", "", enabled))
		return
	}
	secure := auth.IsSecureRequest(r)
	w.Header().Set("Set-Cookie", auth.SetSetupCookie(secret, s.sessionSecret, pendingTTLSeconds, secure))
	sendHTML(w, 200, web.TwoFASetupPage(s.siteTitle, "", secret, enabled))
}

func (s *Server) handleTwoFASetupPost(w http.ResponseWriter, r *http.Request, username string) {
	enabled := s.twoFAEnabled(username)
	if err := r.ParseForm(); err != nil {
		sendHTML(w, 500, web.TwoFASetupPage(s.siteTitle, "请求处理失败", "", enabled))
		return
	}

	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	secret, ok := auth.ValidateSetupToken(cookies[auth.TOTASetupCookie], s.sessionSecret)
	if !ok {
		sendHTML(w, 401, web.TwoFASetupPage(s.siteTitle, "设置会话已过期，请重新开始", "", enabled))
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.VerifyTOTP(secret, code, time.Now()) {
		sendHTML(w, 401, web.TwoFASetupPage(s.siteTitle, "动态码错误或已过期，请重试", secret, enabled))
		return
	}

	user := s.registry.GetUser(username)
	if user == nil {
		sendHTML(w, 404, web.NotFoundPage(s.siteTitle))
		return
	}
	if err := config.UpdateTOTPSecret(user.ConfigPath, secret); err != nil {
		sendHTML(w, 500, web.TwoFASetupPage(s.siteTitle, "保存失败：配置文件不可写", secret, enabled))
		return
	}
	s.registry.Reload()
	w.Header().Add("Set-Cookie", auth.ClearSetupCookie())
	http.Redirect(w, r, "/", http.StatusFound)
}

// twoFAEnabled reports whether the user has a TOTP secret configured.
func (s *Server) twoFAEnabled(username string) bool {
	user := s.registry.GetUser(username)
	return user != nil && user.Config.Auth.TOTPSecret != ""
}

// handleTwoFAQr renders the provisioning QR code for the pending setup secret.
func (s *Server) handleTwoFAQr(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	secret, ok := auth.ValidateSetupToken(cookies[auth.TOTASetupCookie], s.sessionSecret)
	if !ok {
		http.Error(w, "invalid setup", http.StatusBadRequest)
		return
	}
	png, err := qrcode.Encode(auth.TOTPURI(session.UserID, secret), qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "qr error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

// handleTwoFADisable turns off TOTP for the logged-in user.
func (s *Server) handleTwoFADisable(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.Method != "POST" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !auth.SameOriginRequest(r) {
		sendHTML(w, 403, web.NotFoundPage(s.siteTitle))
		return
	}

	user := s.registry.GetUser(session.UserID)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := config.UpdateTOTPSecret(user.ConfigPath, ""); err != nil {
		sendHTML(w, 500, web.LoginPage(s.siteTitle, "关闭失败：配置文件不可写", ""))
		return
	}
	s.registry.Reload()
	http.Redirect(w, r, "/", http.StatusFound)
}
