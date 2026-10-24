package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ogefest/hoarfrost/internal/auth"
)

func (s *Server) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if n, err := s.Auth.CountUsers(r.Context()); err == nil && n == 0 {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", "Sign in", map[string]string{"Next": safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.PostFormValue("next"))
	if !s.limiter.allow("login:" + clientIP(r)) {
		s.renderStatus(w, r, http.StatusTooManyRequests, "login.html", "Sign in", map[string]string{"Next": next, "Error": "Too many attempts, try again in a minute."})
		return
	}
	u, err := s.Auth.Authenticate(r.Context(), r.PostFormValue("login"), r.PostFormValue("password"))
	if err != nil {
		status := http.StatusUnauthorized
		msg := "Invalid login or password."
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			status, msg = http.StatusInternalServerError, "Sign in failed."
			s.Log.Error("login", "err", err)
		}
		s.renderStatus(w, r, status, "login.html", "Sign in", map[string]string{"Next": next, "Error": msg})
		return
	}
	sess, err := s.Auth.CreateSession(r.Context(), u, SessionTTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setCookie(w, sess.ID, int(SessionTTL.Seconds()))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		sess, err := s.Auth.Session(r.Context(), c.Value)
		if err == nil && !constantEqual(r.PostFormValue("csrf"), sess.CSRF) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		_ = s.Auth.DeleteSession(r.Context(), c.Value)
	}
	s.setCookie(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// setupPage creates the first administrator; it is disabled once any user exists.
func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if n, err := s.Auth.CountUsers(r.Context()); err != nil || n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "setup.html", "Create administrator", map[string]string{})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("setup:" + clientIP(r)) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	if n, err := s.Auth.CountUsers(r.Context()); err != nil || n > 0 {
		http.Error(w, "setup already done", http.StatusForbidden)
		return
	}
	if r.PostFormValue("password") != r.PostFormValue("password2") {
		s.renderStatus(w, r, http.StatusBadRequest, "setup.html", "Create administrator", map[string]string{"Error": "Passwords do not match."})
		return
	}
	u, err := s.Auth.CreateUser(r.Context(), r.PostFormValue("login"), r.PostFormValue("password"), auth.RoleAdmin)
	if err != nil {
		s.renderStatus(w, r, http.StatusBadRequest, "setup.html", "Create administrator", map[string]string{"Error": err.Error()})
		return
	}
	sess, err := s.Auth.CreateSession(r.Context(), u, SessionTTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setCookie(w, sess.ID, int(SessionTTL.Seconds()))
	redirect(w, r, "/sources", "Administrator created. Configure your first source.")
}

type accountData struct {
	Tokens   []auth.Token
	NewToken string
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	toks, err := s.Auth.Tokens(r.Context(), userFrom(r).ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "account.html", "Account", accountData{Tokens: toks})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if name == "" {
		name = "token"
	}
	tok, err := s.Auth.CreateToken(r.Context(), userFrom(r), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	toks, _ := s.Auth.Tokens(r.Context(), userFrom(r).ID)
	s.render(w, r, "account.html", "Account", accountData{Tokens: toks, NewToken: tok})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Auth.DeleteToken(r.Context(), id, userFrom(r).ID); err != nil {
		redirectErr(w, r, "/account", err)
		return
	}
	redirect(w, r, "/account", "Token revoked.")
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if _, err := s.Auth.Authenticate(r.Context(), u.Login, r.PostFormValue("current")); err != nil {
		redirectErr(w, r, "/account", errors.New("current password is wrong"))
		return
	}
	if r.PostFormValue("password") != r.PostFormValue("password2") {
		redirectErr(w, r, "/account", errors.New("passwords do not match"))
		return
	}
	if err := s.Auth.SetPassword(r.Context(), u.Login, r.PostFormValue("password")); err != nil {
		redirectErr(w, r, "/account", err)
		return
	}
	sess, err := s.Auth.CreateSession(r.Context(), u, SessionTTL)
	if err == nil {
		s.setCookie(w, sess.ID, int(SessionTTL.Seconds()))
	}
	redirect(w, r, "/account", "Password changed.")
}
