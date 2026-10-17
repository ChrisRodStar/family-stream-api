package api

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"golang.org/x/crypto/argon2"
	"net/http"
	"strings"
	"time"
)

type credentials struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	SetupToken string `json:"setup_token,omitempty"`
}

func passwordKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 32*1024, 2, 32)
}
func (s *Service) authWork(fn func() error) error {
	select {
	case s.authSlots <- struct{}{}:
		defer func() { <-s.authSlots }()
		return fn()
	default:
		return &fault{429, "overloaded"}
	}
}
func (s *Service) account(w http.ResponseWriter, r *http.Request, action string, admin bool) error {
	var c credentials
	if e := body(r, &c); e != nil {
		return e
	}
	c.Username = strings.ToLower(c.Username)
	if !safeID(c.Username) || len(c.Username) > 64 || len(c.Password) < 12 || len(c.Password) > 256 {
		return bad()
	}
	return s.authWork(func() error {
		if action == "login" {
			s.loginMu.Lock()
			if time.Now().After(s.loginReset) {
				s.loginAttempts = 0
				s.loginReset = time.Now().Add(time.Minute)
			}
			s.loginAttempts++
			n := s.loginAttempts
			s.loginMu.Unlock()
			if n > 20 {
				return &fault{429, "login_throttled"}
			}
		}
		var account string
		if action == "setup" || action == "create" {
			if action == "setup" && subtle.ConstantTimeCompare([]byte(c.SetupToken), []byte(s.setup)) != 1 {
				return &fault{401, "unauthorized"}
			}
			salt := make([]byte, 16)
			if _, e := rand.Read(salt); e != nil {
				return e
			}
			key := passwordKey(c.Password, salt)
			account = id()
			e := transaction(r.Context(), s.db, func(tx *sql.Tx) error {
				var n int
				if e := tx.QueryRow("SELECT count(*) FROM accounts").Scan(&n); e != nil {
					return e
				}
				if n >= 32 {
					return &fault{409, "account_limit"}
				}
				if action == "setup" && n != 0 {
					return &fault{409, "already_initialized"}
				}
				_, e := tx.Exec("INSERT INTO accounts VALUES(?,?,?,?,?)", account, c.Username, key, salt, admin)
				if e != nil {
					return &fault{409, "account_exists"}
				}
				return nil
			})
			if e != nil {
				return e
			}
			if action == "create" {
				write(w, 201, map[string]string{"id": account, "username": c.Username})
				return nil
			}
		} else {
			var key, salt []byte
			e := s.db.QueryRowContext(r.Context(), "SELECT id,password,salt FROM accounts WHERE username=?", c.Username).Scan(&account, &key, &salt)
			if errors.Is(e, sql.ErrNoRows) {
				salt = make([]byte, 16)
				key = make([]byte, 32)
			} else if e != nil {
				return e
			}
			derived := passwordKey(c.Password, salt)
			if subtle.ConstantTimeCompare(derived, key) != 1 || account == "" {
				return &fault{401, "unauthorized"}
			}
		}
		token := id()
		expires := time.Now().Add(30 * 24 * time.Hour)
		e := transaction(r.Context(), s.db, func(tx *sql.Tx) error {
			var count int
			if e := tx.QueryRow("SELECT count(*) FROM sessions WHERE account=?", account).Scan(&count); e != nil {
				return e
			}
			if count >= 32 {
				return &fault{409, "session_limit"}
			}
			_, e := tx.Exec("INSERT INTO sessions VALUES(?,?,?)", hash(token), account, expires.Unix())
			return e
		})
		if e != nil {
			return e
		}
		http.SetCookie(w, &http.Cookie{Name: "fs_session", Value: token, Path: "/api/", HttpOnly: true, Secure: strings.HasPrefix(s.cfg.Origin, "https:"), SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 30 * 86400})
		write(w, 200, map[string]any{"token": token, "expires_at": expires, "account_id": account})
		return nil
	})
}
func sessionToken(r *http.Request) string {
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if v == r.Header.Get("Authorization") {
		v = ""
	}
	if v == "" {
		if c, e := r.Cookie("fs_session"); e == nil {
			v = c.Value
		}
	}
	return v
}
func (s *Service) authenticate(r *http.Request) (string, bool, error) {
	token := sessionToken(r)
	if !safeID(token) {
		return "", false, &fault{401, "unauthorized"}
	}
	var account string
	var admin bool
	e := s.db.QueryRowContext(r.Context(), "SELECT a.id,a.admin FROM sessions s JOIN accounts a ON a.id=s.account WHERE s.hash=? AND s.expires>?", hash(token), time.Now().Unix()).Scan(&account, &admin)
	if errors.Is(e, sql.ErrNoRows) {
		return "", false, &fault{401, "unauthorized"}
	}
	return account, admin, e
}
func (s *Service) profile(r *http.Request, account, pid string) (Profile, error) {
	var p Profile
	var b []byte
	e := s.db.QueryRowContext(r.Context(), "SELECT data FROM profiles WHERE id=? AND account=?", pid, account).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return p, missing()
	}
	if e != nil {
		return p, e
	}
	e = decode(b, &p)
	return p, e
}
func (s *Service) changePassword(w http.ResponseWriter, r *http.Request, account string) error {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if e := body(r, &in); e != nil {
		return e
	}
	if len(in.Current) > 256 || len(in.New) < 12 || len(in.New) > 256 {
		return bad()
	}
	return s.authWork(func() error {
		var key, salt []byte
		if e := s.db.QueryRowContext(r.Context(), "SELECT password,salt FROM accounts WHERE id=?", account).Scan(&key, &salt); e != nil {
			return e
		}
		if subtle.ConstantTimeCompare(passwordKey(in.Current, salt), key) != 1 {
			return &fault{401, "unauthorized"}
		}
		nextSalt := make([]byte, 16)
		if _, e := rand.Read(nextSalt); e != nil {
			return e
		}
		next := passwordKey(in.New, nextSalt)
		e := transaction(r.Context(), s.db, func(tx *sql.Tx) error {
			res, e := tx.Exec("UPDATE accounts SET password=?,salt=? WHERE id=? AND password=?", next, nextSalt, account, key)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return conflict()
			}
			_, e = tx.Exec("DELETE FROM sessions WHERE account=?", account)
			return e
		})
		if e != nil {
			return e
		}
		w.WriteHeader(204)
		return nil
	})
}
