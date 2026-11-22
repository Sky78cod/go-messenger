package model

import "time"

// PublicUser never exposes password hashes or session information.
type PublicUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Message struct {
	ID        int64     `json:"id"`
	Room      string    `json:"room"`
	SenderID  int64     `json:"sender_id"`
	Sender    string    `json:"sender"`
	PeerID    int64     `json:"peer_id,omitempty"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Event is sent over WebSocket. The client never gets to choose sender identity.
type Event struct {
	Type    string   `json:"type"`
	Message *Message `json:"message,omitempty"`
	UserID  int64    `json:"user_id,omitempty"`
	Online  bool     `json:"online,omitempty"`
	Users   []int64  `json:"users,omitempty"`
	Room    string   `json:"room,omitempty"`
	PeerID  int64    `json:"peer_id,omitempty"`
	Sender  string   `json:"sender,omitempty"`
	Text    string   `json:"text,omitempty"`
}
