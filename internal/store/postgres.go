package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"example.com/go-messenger/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidPeer = errors.New("пользователь для личного чата не найден")

type Postgres struct{ db *pgxpool.Pool }

type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

func Open(ctx context.Context, databaseURL string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{db: pool}, nil
}

func (p *Postgres) Close() { p.db.Close() }

// ApplySchema is additive so it preserves the user's existing users,
// sessions and general-chat messages from previous project stages.
// This small MVP has no destructive migrations; review changes before deploying.
func (p *Postgres) ApplySchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			username VARCHAR(30) NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL UNIQUE,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id BIGSERIAL PRIMARY KEY,
			username VARCHAR(50) NOT NULL,
			content TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS sender_id BIGINT REFERENCES users(id) ON DELETE SET NULL`,
		`CREATE TABLE IF NOT EXISTS direct_messages (
			id BIGSERIAL PRIMARY KEY,
			sender_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			recipient_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			content TEXT NOT NULL CHECK (char_length(content) BETWEEN 1 AND 1000),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (sender_id <> recipient_id)
		)`,
		`CREATE INDEX IF NOT EXISTS direct_messages_pair_idx ON direct_messages(sender_id, recipient_id, id DESC)`,
		`CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expires_at)`,
	}
	for _, query := range statements {
		if _, err := p.db.Exec(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	var id int64
	err := p.db.QueryRow(ctx,
		`INSERT INTO users(username, password_hash) VALUES ($1, $2) RETURNING id`,
		username, passwordHash).Scan(&id)
	return id, err
}

func (p *Postgres) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := p.db.QueryRow(ctx,
		`SELECT id, username, password_hash FROM users WHERE username=$1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	return u, err
}

func (p *Postgres) CreateSession(ctx context.Context, userID int64, tokenHash string, expiresAt time.Time) error {
	_, err := p.db.Exec(ctx,
		`INSERT INTO sessions(user_id, token_hash, expires_at) VALUES ($1,$2,$3)`,
		userID, tokenHash, expiresAt)
	return err
}

func (p *Postgres) GetUserBySession(ctx context.Context, tokenHash string) (model.PublicUser, error) {
	var user model.PublicUser
	err := p.db.QueryRow(ctx, `SELECT u.id, u.username FROM sessions s
		JOIN users u ON s.user_id=u.id WHERE s.token_hash=$1 AND s.expires_at>NOW()`, tokenHash).
		Scan(&user.ID, &user.Username)
	return user, err
}

func (p *Postgres) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := p.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
	return err
}

func (p *Postgres) PruneSessions(ctx context.Context) error {
	_, err := p.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < NOW()`)
	return err
}

func (p *Postgres) ListUsers(ctx context.Context, excludeID int64) ([]model.PublicUser, error) {
	rows, err := p.db.Query(ctx, `SELECT id, username FROM users WHERE id <> $1 ORDER BY lower(username) LIMIT 500`, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]model.PublicUser, 0)
	for rows.Next() {
		var u model.PublicUser
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (p *Postgres) ListGeneral(ctx context.Context) ([]model.Message, error) {
	rows, err := p.db.Query(ctx, `SELECT id, COALESCE(sender_id, 0), username, content, created_at
		FROM (SELECT id, sender_id, username, content, created_at FROM messages ORDER BY id DESC LIMIT 50) recent
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]model.Message, 0)
	for rows.Next() {
		m := model.Message{Room: "general"}
		if err := rows.Scan(&m.ID, &m.SenderID, &m.Sender, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (p *Postgres) ListDirect(ctx context.Context, meID, peerID int64) ([]model.Message, error) {
	rows, err := p.db.Query(ctx, `SELECT m.id, m.sender_id, u.username, m.recipient_id, m.content, m.created_at
		FROM (SELECT id, sender_id, recipient_id, content, created_at FROM direct_messages
		WHERE (sender_id=$1 AND recipient_id=$2) OR (sender_id=$2 AND recipient_id=$1)
		ORDER BY id DESC LIMIT 50) m
		JOIN users u ON m.sender_id=u.id ORDER BY m.id ASC`, meID, peerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]model.Message, 0)
	for rows.Next() {
		m := model.Message{Room: "direct"}
		if err := rows.Scan(&m.ID, &m.SenderID, &m.Sender, &m.PeerID, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (p *Postgres) InsertGeneral(ctx context.Context, sender model.PublicUser, text string) (model.Message, error) {
	var m model.Message
	m.Room, m.SenderID, m.Sender, m.Text = "general", sender.ID, sender.Username, strings.TrimSpace(text)
	err := p.db.QueryRow(ctx, `INSERT INTO messages(sender_id, username, content) VALUES ($1,$2,$3)
		RETURNING id, created_at`, sender.ID, sender.Username, m.Text).Scan(&m.ID, &m.CreatedAt)
	return m, err
}

func (p *Postgres) InsertDirect(ctx context.Context, sender model.PublicUser, recipientID int64, text string) (model.Message, error) {
	if recipientID == sender.ID || recipientID <= 0 {
		return model.Message{}, ErrInvalidPeer
	}
	var m model.Message
	m.Room, m.SenderID, m.Sender, m.PeerID, m.Text = "direct", sender.ID, sender.Username, recipientID, strings.TrimSpace(text)
	// INSERT...SELECT ensures a valid recipient and avoids sending to nonexisting accounts.
	err := p.db.QueryRow(ctx, `INSERT INTO direct_messages(sender_id, recipient_id, content)
		SELECT $1, id, $3 FROM users WHERE id=$2
		RETURNING id, created_at`, sender.ID, recipientID, m.Text).Scan(&m.ID, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Message{}, ErrInvalidPeer
	}
	return m, err
}
