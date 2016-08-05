package multipass

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/mholt/caddy/caddyhttp/httpserver"

	jose "gopkg.in/square/go-jose.v1"
)

var ErrInvalidToken error = errors.New("invalid token")

type Auth struct {
	*Multipass
	Next httpserver.Handler
}

type Rule struct {
	Basepath  string
	Expires   time.Duration
	Resources []string
	Handles   []string

	SMTPAddr, SMTPUser, SMTPPass string
	MailFrom, MailTmpl           string
}

type Multipass struct {
	Resources []string
	Basepath  string
	SiteAddr  string
	Expires   time.Duration

	sender     Sender
	authorizer Authorizer
	signer     jose.Signer
	key        *rsa.PrivateKey
}

func NewMultipassFromRule(r Rule) (*Multipass, error) {
	m, err := NewMultipass()
	if err != nil {
		return nil, err
	}
	if len(r.Resources) > 0 {
		m.Resources = r.Resources
	}
	if len(r.Basepath) > 0 {
		m.Basepath = r.Basepath
	}
	if r.Expires > 0 {
		m.Expires = r.Expires
	}

	smtpAddr := "localhost:25"
	if len(r.SMTPAddr) > 0 {
		smtpAddr = r.SMTPAddr
	}
	mailTmpl := emailTemplate
	if len(r.MailTmpl) > 0 {
		mailTmpl = r.MailTmpl
	}
	m.sender = NewMailSender(smtpAddr, nil, r.MailFrom, mailTmpl)

	authorizer := &EmailAuthorizer{list: []string{}}
	for _, handle := range r.Handles {
		authorizer.Add(handle)
	}
	m.authorizer = authorizer

	return m, nil
}

func NewMultipass() (*Multipass, error) {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(jose.PS512, pk)
	if err != nil {
		return nil, err
	}
	return &Multipass{
		Resources: []string{"/"},
		Basepath:  "/",
		Expires:   time.Hour * 24,
		key:       pk,
		signer:    signer,
	}, nil
}

// Claims are part of the JSON web token
type Claims struct {
	Handle    string   `json:"handle"`
	Resources []string `json:"resources"`
	Expires   int64    `json:"exp"`
}

func (m *Multipass) AccessToken(handle string) (tokenStr string, err error) {
	exp := time.Now().Add(m.Expires)
	claims := &Claims{
		Handle:    handle,
		Resources: m.Resources,
		Expires:   exp.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	jws, err := m.signer.Sign(payload)
	if err != nil {
		return "", err
	}

	return jws.CompactSerialize()
}

func (m *Multipass) LoginURL(u url.URL, tokenStr string) url.URL {
	u.Path = path.Join(m.Basepath, "login")
	v := url.Values{}
	v.Set("token", tokenStr)
	u.RawQuery = v.Encode()

	return u
}

func loginHandler(w http.ResponseWriter, r *http.Request, m *Multipass) (int, error) {
	if r.Method == "POST" {
		r.ParseForm()
		handle := r.PostForm.Get("handle")
		if len(handle) == 0 {
			loc := path.Join(m.Basepath, "login")
			http.Redirect(w, r, loc, http.StatusSeeOther)
			return http.StatusSeeOther, nil
		}
		switch m.authorizer.IsAuthorized(handle) {
		case true:
			token, err := m.AccessToken(handle)
			if err != nil {
				log.Print(err)
			}
			siteURL, err := url.Parse(m.SiteAddr)
			if err != nil {
				log.Fatal(err)
			}
			loginURL := m.LoginURL(*siteURL, token)
			if err := m.sender.Send(handle, loginURL.String()); err != nil {
				log.Print(err)
			}
		}
		w.Header().Add("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("A login link has been sent to user with handle " + handle + " if your handle is authorized"))
		return http.StatusOK, nil
	}
	if r.Method == "GET" {
		if tokenStr := r.URL.Query().Get("token"); len(tokenStr) > 0 {
			cookie := &http.Cookie{
				Name:  "jwt_token",
				Value: tokenStr,
				Path:  "/",
			}
			http.SetCookie(w, cookie)
			r.URL.Path = ""
			r.URL.RawQuery = ""
			http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
			return http.StatusSeeOther, nil
		}
		w.Header().Add("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body><form action=" + r.URL.Path + " method=POST><input type=text name=handle /><input type=submit></form></body></html>"))
		return http.StatusOK, nil
	}
	return http.StatusMethodNotAllowed, nil
}

func loginformHandler(w http.ResponseWriter, r *http.Request, m *Multipass) (int, error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`
<html><body>
<form action="` + path.Join(m.Basepath, "/login") + `" method=POST>
<input type=hidden name=url value="` + r.URL.String() + `"/>
<input type=text name=handle />
<input type=submit>
</form></body></html>
`))
	return http.StatusOK, nil
}

func signoutHandler(w http.ResponseWriter, r *http.Request, m *Multipass) (int, error) {
	if cookie, err := r.Cookie("jwt_token"); err == nil {
		cookie.Expires = time.Now().AddDate(-1, 0, 0)
		cookie.MaxAge = -1
		cookie.Path = "/"
		http.SetCookie(w, cookie)
	}
	loc := path.Join(m.Basepath, "login")
	http.Redirect(w, r, loc, http.StatusSeeOther)
	return http.StatusSeeOther, nil
}

func publickeyHandler(w http.ResponseWriter, r *http.Request, m *Multipass) (int, error) {
	data, err := x509.MarshalPKIXPublicKey(&m.key.PublicKey)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	block := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: data,
	}
	w.Header().Set("Content-Type", "application/pkix-cert")
	if err := pem.Encode(w, block); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}

func tokenHandler(w http.ResponseWriter, r *http.Request, m *Multipass) (int, error) {
	// Extract token from HTTP header, query parameter or cookie
	tokenStr, err := extractToken(r)
	if err != nil {
		return http.StatusUnauthorized, ErrInvalidToken
	}
	var claims *Claims
	if claims, err = validateToken(tokenStr, m.key.PublicKey); err != nil {
		return http.StatusUnauthorized, ErrInvalidToken
	}
	// Authorize handle claim
	if ok := m.authorizer.IsAuthorized(claims.Handle); !ok {
		return http.StatusUnauthorized, ErrInvalidToken
	}
	// Verify path claim
	var match bool
	for _, path := range claims.Resources {
		if httpserver.Path(r.URL.Path).Matches(path) {
			match = true
			continue
		}
	}
	if !match {
		return http.StatusUnauthorized, ErrInvalidToken
	}
	return http.StatusOK, nil
}

func (a *Auth) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	m := a.Multipass
	var pathMatch bool
	for _, path := range m.Resources {
		if httpserver.Path(r.URL.Path).Matches(path) {
			pathMatch = true
			continue
		}
	}
	if !pathMatch {
		return a.Next.ServeHTTP(w, r)
	}

	switch r.URL.Path {
	case path.Join(m.Basepath, "pub.cer"):
		return publickeyHandler(w, r, m)
	case path.Join(m.Basepath, "login"):
		return loginHandler(w, r, m)
	case path.Join(m.Basepath, "signout"):
		return signoutHandler(w, r, m)
	default:
		if code, err := tokenHandler(w, r, m); err != nil {
			w.WriteHeader(code)
			return loginformHandler(w, r, m)
		}
	}
	return a.Next.ServeHTTP(w, r)
}

// extractToken returns the JWT token embedded in the given request.
// JWT tokens can be embedded in the header prefixed with "Bearer ", with a
// "token" key query parameter or a cookie named "jwt_token".
func extractToken(r *http.Request) (string, error) {
	//from header
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if len(h) > 7 {
			return h[7:], nil
		}
	}

	//from query parameter
	if token := r.URL.Query().Get("token"); len(token) > 0 {
		return token, nil
	}

	//from cookie
	if cookie, err := r.Cookie("jwt_token"); err == nil {
		return cookie.Value, nil
	}

	return "", fmt.Errorf("no token found")
}

func validateToken(token string, key rsa.PublicKey) (*Claims, error) {
	claims := &Claims{}

	// Verify token signature
	payload, err := verifyToken(token, key)
	if err != nil {
		return nil, err
	}
	// Unmarshal token claims
	if err := json.Unmarshal(payload, claims); err != nil {
		return nil, err
	}
	// Verify expire claim
	if time.Unix(claims.Expires, 0).Before(time.Now()) {
		return nil, errors.New("Token expired")
	}
	return claims, nil
}

func verifyToken(token string, key rsa.PublicKey) ([]byte, error) {
	var data []byte

	obj, err := jose.ParseSigned(token)
	if err != nil {
		return data, err
	}
	data, err = obj.Verify(&key)
	if err != nil {
		return data, err
	}
	return data, nil
}
