package api

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

// totpIssuer is the account label shown by authenticator apps.
const totpIssuer = "İzlek"

var okBody = map[string]bool{"ok": true}

func errWeakPassword() *apiError {
	return errCode(http.StatusBadRequest, "weak_password", "Parola en az 10 karakter olmalıdır")
}

func errInvalidCredentials(status int, message string) *apiError {
	return errCode(status, "invalid_credentials", message)
}

func rateLimited(wait float64) *apiError {
	secs := int(math.Ceil(wait))
	if secs < 1 {
		secs = 1
	}
	return &apiError{Status: http.StatusTooManyRequests, Code: "rate_limited",
		Message: "Çok fazla deneme yapıldı; bir dakika sonra yeniden deneyin", RetryAfter: secs}
}

// GET /api/setup
func (s *Server) getSetup(c *reqCtx) (any, error) {
	n, err := db.CountUsers(c.ctx(), s.db.R())
	if err != nil {
		return nil, err
	}
	return map[string]bool{"needs_setup": n == 0}, nil
}

// POST /api/setup — only while there is no user at all.
func (s *Server) postSetup(c *reqCtx) (any, error) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		OrgName  string `json:"org_name"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	alreadySetUp := errCode(http.StatusConflict, "already_set_up", "Kurulum zaten tamamlanmış")
	if n, err := db.CountUsers(c.ctx(), s.db.R()); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, alreadySetUp
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if auth.CheckPassword(in.Password) != nil {
		return nil, errWeakPassword()
	}
	orgName, err := cleanText("org_name", in.OrgName, 1, 100)
	if err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(c.ctx(), in.Password)
	if err != nil {
		return nil, err
	}
	var cookie string
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		// Re-checked under the write lock: two racing setups cannot both win.
		if n, err := db.CountUsers(c.ctx(), tx); err != nil {
			return err
		} else if n > 0 {
			return alreadySetUp
		}
		uid, err := db.CreateUser(c.ctx(), tx, email, hash, c.nowMs())
		if err != nil {
			return err
		}
		orgID, err := db.CreateOrg(c.ctx(), tx, orgName, c.nowMs())
		if err != nil {
			return err
		}
		if err := db.AddMember(c.ctx(), tx, orgID, uid, db.RoleOwner, c.nowMs()); err != nil {
			return err
		}
		s.mAudit.Inc()
		if err := db.InsertAudit(c.ctx(), tx, c.nowMs(), orgID, db.UserActor(uid), "setup", strconv.FormatInt(orgID, 10),
			map[string]any{"org_name": orgName, "email": email}); err != nil {
			return err
		}
		cookie, err = c.openSession(tx, uid)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.setSessionCookie(c.w, cookie)
	return okBody, nil
}

// POST /api/auth/login
func (s *Server) login(c *reqCtx) (any, error) {
	var in struct {
		Email    string     `json:"email"`
		Password string     `json:"password"`
		TOTP     flexString `json:"totp"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	cookie, err := s.passwordLogin(c.ctx(), s.clientIP(c.r), c.now, in.Email, in.Password, string(in.TOTP))
	if err != nil {
		return nil, err
	}
	s.setSessionCookie(c.w, cookie)
	return okBody, nil
}

// passwordLogin checks an e-mail address, a password and, for accounts with
// a second factor, a TOTP code, and opens a session; it returns the value of
// the session cookie. It is the one implementation of signing in: the JSON
// API uses it and so does the sign-in form of the OAuth consent page, so
// both share the throttle, the timing behaviour and the audit entry.
func (s *Server) passwordLogin(ctx context.Context, ip string, now time.Time, emailIn, password, totp string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(emailIn))
	if len(email) > 254 {
		email = email[:254]
	}
	pairKey, ipKey := ip+"\x00"+email, ip
	if blocked, wait := s.loginPair.Blocked(pairKey); blocked {
		s.mLogins.With("rate_limited").Inc()
		return "", rateLimited(wait.Seconds())
	}
	if blocked, wait := s.loginIP.Blocked(ipKey); blocked {
		s.mLogins.With("rate_limited").Inc()
		return "", rateLimited(wait.Seconds())
	}
	fail := func(result string, e *apiError) (string, error) {
		s.loginPair.Add(pairKey)
		s.loginIP.Add(ipKey)
		s.mLogins.With(result).Inc()
		return "", e
	}
	badCredentials := errInvalidCredentials(http.StatusUnauthorized, "E-posta ya da parola hatalı")

	user, err := db.UserByEmail(ctx, s.db.R(), email)
	if errors.Is(err, db.ErrNotFound) {
		// Same cost as a real check: timing must not reveal which
		// addresses have an account.
		s.hasher.VerifyDummy(ctx, password)
		return fail("invalid_credentials", badCredentials)
	}
	if err != nil {
		return "", err
	}
	ok, rehash, err := s.hasher.Verify(ctx, user.PasswordHash, password)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		s.log.Error("stored password hash is unusable", "user", user.ID, "err", err.Error())
		return fail("invalid_credentials", badCredentials)
	}
	if !ok {
		return fail("invalid_credentials", badCredentials)
	}
	var totpStep int64
	if user.TOTPEnabled {
		code := strings.TrimSpace(totp)
		if code == "" {
			// Not counted as a failure: this is the first half of the
			// normal two-step login, and the password was right.
			s.mLogins.With("totp_required").Inc()
			return "", errCode(http.StatusUnauthorized, "totp_required", "Doğrulama kodu gerekli")
		}
		// A numeric JSON value loses its leading zeros.
		if len(code) < auth.TOTPDigits {
			code = strings.Repeat("0", auth.TOTPDigits-len(code)) + code
		}
		step, valid := auth.VerifyTOTP(user.TOTPSecret, code, now, user.TOTPLastStep)
		if !valid {
			return fail("invalid_totp", errCode(http.StatusUnauthorized, "invalid_totp", "Doğrulama kodu hatalı"))
		}
		totpStep = step
	}
	var newHash string
	if rehash {
		if newHash, err = s.hasher.Hash(ctx, password); err != nil {
			newHash = ""
		}
	}
	var cookie string
	replayed := false
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if totpStep != 0 {
			fresh, err := db.AdvanceTOTPStep(ctx, tx, user.ID, totpStep)
			if err != nil {
				return err
			}
			if !fresh { // the same code was used by a concurrent login
				replayed = true
				return nil
			}
		}
		if newHash != "" {
			if err := db.SetPassword(ctx, tx, user.ID, newHash); err != nil {
				return err
			}
		}
		s.mAudit.Inc()
		if err := db.InsertAudit(ctx, tx, now.UnixMilli(), 0, db.UserActor(user.ID), "auth.login", strconv.FormatInt(user.ID, 10), nil); err != nil {
			return err
		}
		var err error
		cookie, err = openSession(ctx, tx, user.ID, now)
		return err
	})
	if err != nil {
		return "", err
	}
	if replayed {
		return fail("invalid_totp", errCode(http.StatusUnauthorized, "invalid_totp", "Doğrulama kodu hatalı"))
	}
	s.mLogins.With("ok").Inc()
	return cookie, nil
}

// POST /api/auth/logout
func (s *Server) logout(c *reqCtx) (any, error) {
	err := s.db.Write(c.ctx(), func(tx *sql.Tx) error { return db.DeleteSession(c.ctx(), tx, c.session.IDHash) })
	if err != nil {
		return nil, err
	}
	s.clearSessionCookie(c.w)
	return okBody, nil
}

// GET /api/me
func (s *Server) me(c *reqCtx) (any, error) {
	orgs, err := db.UserOrgs(c.ctx(), s.db.R(), c.user().ID)
	if err != nil {
		return nil, err
	}
	u := c.user()
	return map[string]any{
		"user": map[string]any{"id": u.ID, "email": u.Email, "totp_enabled": u.TOTPEnabled},
		"orgs": orgs,
		"csrf": c.session.CSRF,
	}, nil
}

// confirmPassword re-checks the session user's password for a sensitive
// change. Failures are throttled per user and answered 403 — not 401, which
// would tell the UI that the session itself is gone.
func (c *reqCtx) confirmPassword(password, message string) error {
	key := "user:" + strconv.FormatInt(c.user().ID, 10)
	if blocked, wait := c.s.reauth.Blocked(key); blocked {
		return rateLimited(wait.Seconds())
	}
	ok, _, err := c.s.hasher.Verify(c.ctx(), c.user().PasswordHash, password)
	if err != nil && c.ctx().Err() != nil {
		return c.ctx().Err()
	}
	if err != nil || !ok {
		c.s.reauth.Add(key)
		return errInvalidCredentials(http.StatusForbidden, message)
	}
	return nil
}

// POST /api/me/password
func (s *Server) changePassword(c *reqCtx) (any, error) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if auth.CheckPassword(in.New) != nil {
		return nil, errWeakPassword()
	}
	if err := c.confirmPassword(in.Current, "Mevcut parola hatalı"); err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(c.ctx(), in.New)
	if err != nil {
		return nil, err
	}
	uid := c.user().ID
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.SetPassword(c.ctx(), tx, uid, hash); err != nil {
			return err
		}
		// Whoever else holds a session of this account is signed out, and
		// applications that were authorised to act for it must be approved
		// again: a password is changed when it may have been in the wrong
		// hands, and so may anything approved with it.
		if err := db.DeleteOtherSessions(c.ctx(), tx, uid, c.session.IDHash); err != nil {
			return err
		}
		revoked, err := db.RevokeUserOAuthGrants(c.ctx(), tx, uid, c.nowMs(), "password_change")
		if err != nil {
			return err
		}
		tokens, err := db.DeleteUserPersonalTokens(c.ctx(), tx, uid)
		if err != nil {
			return err
		}
		var detail any
		if revoked > 0 || tokens > 0 {
			d := map[string]any{}
			if revoked > 0 {
				d["oauth_grants_revoked"] = revoked
			}
			if tokens > 0 {
				d["personal_tokens_revoked"] = tokens
			}
			detail = d
		}
		return c.audit(tx, 0, "user.password", strconv.FormatInt(uid, 10), detail)
	})
	if err != nil {
		return nil, err
	}
	return okBody, nil
}

// POST /api/me/totp/begin
func (s *Server) totpBegin(c *reqCtx) (any, error) {
	if c.user().TOTPEnabled {
		return nil, errCode(http.StatusConflict, "totp_already_enabled", "İki adımlı doğrulama zaten açık")
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		return nil, err
	}
	uid := c.user().ID
	if err := s.db.Write(c.ctx(), func(tx *sql.Tx) error { return db.SetTOTPPending(c.ctx(), tx, uid, secret) }); err != nil {
		return nil, err
	}
	return map[string]string{"secret": secret, "otpauth_url": auth.OTPAuthURL(totpIssuer, c.user().Email, secret)}, nil
}

// POST /api/me/totp/enable
func (s *Server) totpEnable(c *reqCtx) (any, error) {
	var in struct {
		Code flexString `json:"code"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	u := c.user()
	if u.TOTPEnabled {
		return nil, errCode(http.StatusConflict, "totp_already_enabled", "İki adımlı doğrulama zaten açık")
	}
	if u.TOTPPending == "" {
		return nil, errCode(http.StatusBadRequest, "totp_not_started", "Önce kurulumu başlatın")
	}
	key := "user:" + strconv.FormatInt(u.ID, 10)
	if blocked, wait := s.reauth.Blocked(key); blocked {
		return nil, rateLimited(wait.Seconds())
	}
	code := strings.TrimSpace(string(in.Code))
	if len(code) > 0 && len(code) < auth.TOTPDigits {
		code = strings.Repeat("0", auth.TOTPDigits-len(code)) + code
	}
	step, ok := auth.VerifyTOTP(u.TOTPPending, code, c.now, 0)
	if !ok {
		s.reauth.Add(key)
		return nil, errCode(http.StatusBadRequest, "invalid_totp", "Doğrulama kodu hatalı")
	}
	err := s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.EnableTOTP(c.ctx(), tx, u.ID, step); err != nil {
			return err
		}
		// Sessions opened without the second factor end here, and so do
		// the applications authorised from such sessions.
		if err := db.DeleteOtherSessions(c.ctx(), tx, u.ID, c.session.IDHash); err != nil {
			return err
		}
		revoked, err := db.RevokeUserOAuthGrants(c.ctx(), tx, u.ID, c.nowMs(), "totp_enable")
		if err != nil {
			return err
		}
		tokens, err := db.DeleteUserPersonalTokens(c.ctx(), tx, u.ID)
		if err != nil {
			return err
		}
		var detail any
		if revoked > 0 || tokens > 0 {
			d := map[string]any{}
			if revoked > 0 {
				d["oauth_grants_revoked"] = revoked
			}
			if tokens > 0 {
				d["personal_tokens_revoked"] = tokens
			}
			detail = d
		}
		return c.audit(tx, 0, "user.totp_enable", strconv.FormatInt(u.ID, 10), detail)
	})
	if err != nil {
		return nil, err
	}
	return okBody, nil
}

// POST /api/me/totp/disable
func (s *Server) totpDisable(c *reqCtx) (any, error) {
	var in struct {
		Password string `json:"password"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if err := c.confirmPassword(in.Password, "Parola hatalı"); err != nil {
		return nil, err
	}
	uid := c.user().ID
	err := s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.DisableTOTP(c.ctx(), tx, uid); err != nil {
			return err
		}
		return c.audit(tx, 0, "user.totp_disable", strconv.FormatInt(uid, 10), nil)
	})
	if err != nil {
		return nil, err
	}
	return okBody, nil
}
