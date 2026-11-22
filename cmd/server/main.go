package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/go-messenger/internal/auth"
	"example.com/go-messenger/internal/chat"
	"example.com/go-messenger/internal/model"
	"example.com/go-messenger/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,30}$`)

type Server struct {
	db            *store.Postgres
	hub           *chat.Hub
	secureCookies bool
	loginAttempts *attemptLimiter
}

type attemptLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = make(map[string][]time.Time)
	}
	// Bound memory: discard old windows, including for attackers using many IDs.
	if len(l.hits) > 5000 {
		for k, values := range l.hits {
			if len(values) == 0 || time.Since(values[len(values)-1]) > time.Minute {
				delete(l.hits, k)
			}
		}
		if len(l.hits) > 5000 {
			return false
		}
	}
	now := time.Now()
	var recent []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 20 {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL не задан (см. README.md)")
	}
	startupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := store.Open(startupCtx, databaseURL)
	if err != nil {
		log.Fatalf("не удалось подключиться к PostgreSQL: %v", err)
	}
	defer db.Close()
	if err := db.ApplySchema(startupCtx); err != nil {
		log.Fatalf("ошибка схемы БД: %v", err)
	}

	hub := chat.NewHub()
	go hub.Run()
	s := &Server{db: db, hub: hub, secureCookies: os.Getenv("APP_ENV") == "production", loginAttempts: &attemptLimiter{}}
	go s.cleanupSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/users", s.users)
	mux.HandleFunc("GET /api/messages", s.messages)
	mux.HandleFunc("GET /ws", s.ws)
	mux.Handle("GET /", http.FileServer(http.Dir("web")))

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr: addr, Handler: withSecurityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second,
	}
	log.Printf("Go Messenger запущен на %s", addr)
	log.Fatal(server.ListenAndServe())
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("отправка JSON: %v", err)
	}
}
func apiError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func readCredentials(w http.ResponseWriter, r *http.Request) (string, string, error) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return "", "", errors.New("Content-Type должен быть application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return "", "", err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", "", errors.New("лишние данные в JSON")
	}
	return input.Username, input.Password, nil
}

// Browsers send Origin for fetch POSTs. Also reject explicit cross-site Fetch Metadata;
// CLI requests without those headers are allowed for local testing.
func sameOriginMutation(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
	}
	return true
}

func remoteIP(r *http.Request) string {
	// Only RemoteAddr is trusted. Behind a proxy this yields the proxy's IP;
	// the limiter still protects the service but shares a window per proxy.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(r) {
		apiError(w, 403, "Недопустимый источник запроса")
		return
	}
	if !s.loginAttempts.allow("register:" + remoteIP(r)) {
		apiError(w, 429, "Слишком много попыток, повторите позже")
		return
	}
	username, password, err := readCredentials(w, r)
	if err != nil {
		apiError(w, 400, "Неверный JSON")
		return
	}
	if !usernamePattern.MatchString(username) || len(password) < 12 || len(password) > 128 {
		apiError(w, 400, "Имя: 3–30 латинских символов/цифр/_. Пароль: 12–128 байт")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		apiError(w, 500, "Ошибка сервера")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	id, err := s.db.CreateUser(ctx, username, hash)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			apiError(w, 409, "Имя пользователя занято")
			return
		}
		log.Printf("регистрация: %v", err)
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	user := model.PublicUser{ID: id, Username: username}
	if err := s.startSession(w, r, user.ID); err != nil {
		log.Printf("новый пользователь создан, но сессия не создана: %v", err)
		apiError(w, 500, "Аккаунт создан, но автоматический вход не удался; войдите вручную")
		return
	}
	writeJSON(w, 201, user)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(r) {
		apiError(w, 403, "Недопустимый источник запроса")
		return
	}
	if !s.loginAttempts.allow("login:" + remoteIP(r)) {
		apiError(w, 429, "Слишком много попыток, повторите позже")
		return
	}
	username, password, err := readCredentials(w, r)
	if err != nil {
		apiError(w, 400, "Неверный JSON")
		return
	}
	if !usernamePattern.MatchString(username) || len(password) == 0 || len(password) > 128 {
		apiError(w, 401, "Неверный логин или пароль")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, err := s.db.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, 401, "Неверный логин или пароль")
		return
	}
	if err != nil {
		log.Printf("поиск пользователя: %v", err)
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	ok, err := auth.CheckPassword(password, u.PasswordHash)
	if err != nil {
		log.Printf("повреждённый хеш пользователя %d: %v", u.ID, err)
		apiError(w, 500, "Ошибка сервера")
		return
	}
	if !ok {
		apiError(w, 401, "Неверный логин или пароль")
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		log.Printf("создание сессии: %v", err)
		apiError(w, 500, "Ошибка сервера")
		return
	}
	writeJSON(w, 200, model.PublicUser{ID: u.ID, Username: u.Username})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, id int64) error {
	token, hash, err := auth.GenerateSessionToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(7 * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.db.CreateSession(ctx, id, hash, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: "session", Value: token, Path: "/", HttpOnly: true,
		Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		Expires: expires, MaxAge: 7 * 24 * 60 * 60,
	})
	return nil
}

func (s *Server) currentUser(r *http.Request) (model.PublicUser, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return model.PublicUser{}, pgx.ErrNoRows
	}
	if err := auth.ValidateSessionToken(cookie.Value); err != nil {
		return model.PublicUser{}, pgx.ErrNoRows
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	return s.db.GetUserBySession(ctx, auth.SessionTokenHash(cookie.Value))
}

func (s *Server) authenticated(w http.ResponseWriter, r *http.Request) (model.PublicUser, bool) {
	u, err := s.currentUser(r)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, 401, "Не авторизован")
		return model.PublicUser{}, false
	}
	if err != nil {
		log.Printf("проверка сессии: %v", err)
		apiError(w, 500, "Ошибка сервера")
		return model.PublicUser{}, false
	}
	return u, true
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if u, ok := s.authenticated(w, r); ok {
		writeJSON(w, 200, u)
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(r) {
		apiError(w, 403, "Недопустимый источник запроса")
		return
	}
	if cookie, err := r.Cookie("session"); err == nil && auth.ValidateSessionToken(cookie.Value) == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.db.DeleteSession(ctx, auth.SessionTokenHash(cookie.Value)); err != nil {
			log.Printf("удаление сессии: %v", err)
			apiError(w, 500, "Ошибка сервера")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticated(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	users, err := s.db.ListUsers(ctx, u.ID)
	if err != nil {
		log.Printf("список пользователей: %v", err)
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	writeJSON(w, 200, users)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticated(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result []model.Message
	var err error
	if peerStr := r.URL.Query().Get("peer_id"); peerStr != "" {
		id, parseErr := strconv.ParseInt(peerStr, 10, 64)
		if parseErr != nil || id < 1 || id == u.ID {
			apiError(w, 400, "Неверный собеседник")
			return
		}
		result, err = s.db.ListDirect(ctx, u.ID, id)
	} else {
		result, err = s.db.ListGeneral(ctx)
	}
	if err != nil {
		log.Printf("история сообщений: %v", err)
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUser(r)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, 401, "Не авторизован")
		return
	}
	if err != nil {
		log.Printf("ws auth: %v", err)
		apiError(w, 500, "Ошибка сервера")
		return
	}
	cookie, err := r.Cookie("session")
	if err != nil {
		apiError(w, 401, "Не авторизован")
		return
	}
	chat.ServeWS(s.hub, s.db, u, auth.SessionTokenHash(cookie.Value), w, r)
}

func (s *Server) cleanupSessions() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.db.PruneSessions(ctx); err != nil {
			log.Printf("очистка сессий: %v", err)
		}
		cancel()
	}
}
