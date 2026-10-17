package rss

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

//go:embed templates/*.html
var templateFiles embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{"fallback": func(value, other string) string {
	if value == "" {
		return other
	}
	return value
}}).ParseFS(templateFiles, "templates/*.html"))

// VerifyPassword 支持 Werkzeug 旧 scrypt 和 PBKDF2 密码，验证时不改写数据库。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 {
		return false
	}
	method := strings.Split(parts[0], ":")
	expected, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	var actual []byte
	switch method[0] {
	case "scrypt":
		if len(method) != 4 {
			return false
		}
		n, e1 := strconv.Atoi(method[1])
		r, e2 := strconv.Atoi(method[2])
		p, e3 := strconv.Atoi(method[3])
		if e1 != nil || e2 != nil || e3 != nil || n < 2 || n > 1<<20 || r <= 0 || r > 32 || p <= 0 || p > 32 || int64(n)*int64(r) > 1<<22 {
			return false
		}
		actual, err = scrypt.Key([]byte(password), []byte(parts[1]), n, r, p, 64)
	case "pbkdf2":
		if len(method) < 2 || len(method) > 3 {
			return false
		}
		rounds := 600000
		if len(method) == 3 {
			rounds, err = strconv.Atoi(method[2])
		}
		if err != nil || rounds <= 0 || rounds > 10000000 {
			return false
		}
		var hf func() hash.Hash
		switch method[1] {
		case "sha1":
			hf = sha1.New
		case "sha224":
			hf = sha256.New224
		case "sha256":
			hf = sha256.New
		case "sha384":
			hf = sha512.New384
		case "sha512":
			hf = sha512.New
		default:
			return false
		}
		actual = pbkdf2.Key([]byte(password), []byte(parts[1]), rounds, hf().Size(), hf)
	default:
		return false
	}
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	encoded := hex.EncodeToString(salt)
	key, err := scrypt.Key([]byte(password), []byte(encoded), 32768, 8, 1, 64)
	if err != nil {
		return "", err
	}
	return "scrypt:32768:8:1$" + encoded + "$" + hex.EncodeToString(key), nil
}

type session struct {
	Username string `json:"sub"`
	Expires  int64  `json:"exp"`
}

func (s *Server) cookie(w http.ResponseWriter, r *http.Request, user string) {
	payload, _ := json.Marshal(session{user, time.Now().Add(24 * time.Hour).Unix()})
	part := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(part))
	http.SetCookie(w, &http.Cookie{Name: "access_token", Value: part + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
}
func (s *Server) current(r *http.Request) string {
	cookie, err := r.Cookie("access_token")
	if err != nil {
		return ""
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return ""
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(parts[0]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	var sess session
	if json.Unmarshal(data, &sess) != nil || sess.Expires <= time.Now().Unix() {
		return ""
	}
	var n int
	if s.store.DB.GetContext(r.Context(), &n, "SELECT COUNT(*) FROM users WHERE username = ?", sess.Username) != nil || n == 0 {
		return ""
	}
	return sess.Username
}
func (s *Server) require(w http.ResponseWriter, r *http.Request) bool {
	if s.current(r) != "" {
		return true
	}
	failure(w, 401, errors.New("未登录或会话已过期"))
	return false
}
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pages.ExecuteTemplate(w, name, data)
}
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	target := "/rss/dashboard"
	if s.current(r) == "" {
		target = "/login"
	}
	http.Redirect(w, r, target, 302)
}
func (s *Server) userCount(r *http.Request) (int, error) {
	var count int
	err := s.store.DB.GetContext(r.Context(), &count, "SELECT COUNT(*) FROM users")
	return count, err
}
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.current(r) != "" {
		http.Redirect(w, r, "/", 302)
		return
	}
	n, err := s.userCount(r)
	if err != nil {
		failure(w, 500, err)
		return
	}
	if n == 0 {
		http.Redirect(w, r, "/register", 302)
		return
	}
	s.render(w, 200, "login.html", map[string]any{})
}
func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	n, err := s.userCount(r)
	if err != nil {
		failure(w, 500, err)
		return
	}
	if n > 0 {
		http.Redirect(w, r, "/login", 302)
		return
	}
	s.render(w, 200, "register.html", map[string]any{})
}
func (s *Server) verify(r *http.Request, user, password string) bool {
	var stored string
	err := s.store.DB.GetContext(r.Context(), &stored, "SELECT password FROM users WHERE username = ? LIMIT 1", user)
	return err == nil && VerifyPassword(stored, password)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		failure(w, 400, err)
		return
	}
	user := r.FormValue("username")
	if !s.verify(r, user, r.FormValue("password")) {
		s.render(w, 401, "login.html", map[string]any{"Error": "用户名或密码错误"})
		return
	}
	s.cookie(w, r, user)
	http.Redirect(w, r, "/", 302)
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		failure(w, 400, err)
		return
	}
	user, password := strings.TrimSpace(r.FormValue("username")), r.FormValue("password")
	if user == "" || password == "" || password != r.FormValue("confirm_password") {
		s.render(w, 400, "register.html", map[string]any{"Error": "用户名、密码或两次确认密码无效"})
		return
	}
	encoded, err := hashPassword(password)
	if err != nil {
		failure(w, 500, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.store.DB.ExecContext(r.Context(), "INSERT INTO users(username,password) SELECT ?,? WHERE NOT EXISTS (SELECT 1 FROM users)", user, encoded)
	if err != nil {
		failure(w, 500, err)
		return
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		failure(w, 403, errors.New("已有用户，注册已关闭"))
		return
	}
	s.cookie(w, r, user)
	http.Redirect(w, r, "/", 302)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "access_token", Value: "", MaxAge: -1, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", 302)
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		failure(w, 400, err)
		return
	}
	user := s.current(r)
	password := r.FormValue("new_password")
	if len([]rune(password)) < 8 {
		failure(w, 200, errors.New("新密码长度必须至少为8个字符"))
		return
	}
	if password != r.FormValue("confirm_password") {
		failure(w, 200, errors.New("新密码和确认密码不一致"))
		return
	}
	if !s.verify(r, user, r.FormValue("current_password")) {
		failure(w, 200, errors.New("当前密码不正确"))
		return
	}
	encoded, err := hashPassword(password)
	if err == nil {
		_, err = s.store.DB.ExecContext(r.Context(), "UPDATE users SET password = ? WHERE username = ?", encoded, user)
	}
	if err != nil {
		failure(w, 500, err)
		return
	}
	reply(w, 200, map[string]any{"success": true, "message": "密码修改成功"})
}
