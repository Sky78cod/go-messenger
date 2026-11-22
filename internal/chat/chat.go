package chat

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"example.com/go-messenger/internal/model"
	"example.com/go-messenger/internal/store"
	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 70 * time.Second
	pingPeriod = 25 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize: 1024, WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return false
		}
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
	},
}

type Client struct {
	user        model.PublicUser
	sessionHash string
	conn        *websocket.Conn
	send        chan model.Event
	hub         *Hub
	store       *store.Postgres
}

type Hub struct {
	join    chan *Client
	leave   chan *Client
	events  chan model.Event
	clients map[*Client]struct{}
	count   map[int64]int
}

func NewHub() *Hub {
	return &Hub{
		join: make(chan *Client), leave: make(chan *Client),
		events:  make(chan model.Event, 256),
		clients: make(map[*Client]struct{}), count: make(map[int64]int),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case c := <-h.join:
			h.clients[c] = struct{}{}
			h.count[c.user.ID]++
			online := make([]int64, 0, len(h.count))
			for id := range h.count {
				online = append(online, id)
			}
			h.deliver(c, model.Event{Type: "online_snapshot", Users: online})
			if h.count[c.user.ID] == 1 {
				h.sendPresence(c.user.ID, true)
			}
		case c := <-h.leave:
			h.remove(c)
		case event := <-h.events:
			for c := range h.clients {
				if event.Type == "message" && event.Message != nil && event.Message.Room == "direct" {
					m := event.Message
					if c.user.ID != m.SenderID && c.user.ID != m.PeerID {
						continue
					}
				} else if event.Type == "typing" && event.Room == "direct" {
					if c.user.ID != event.PeerID {
						continue
					}
				}
				h.deliver(c, event)
			}
		}
	}
}

func (h *Hub) deliver(c *Client, event model.Event) {
	select {
	case c.send <- event:
	default:
		// Slow clients are disconnected so they cannot block other users.
		_ = c.conn.Close()
		h.remove(c)
	}
}

func (h *Hub) remove(c *Client) {
	if _, exists := h.clients[c]; !exists {
		return
	}
	delete(h.clients, c)
	close(c.send)
	h.count[c.user.ID]--
	if h.count[c.user.ID] == 0 {
		delete(h.count, c.user.ID)
		h.sendPresence(c.user.ID, false)
	}
}

func (h *Hub) sendPresence(id int64, online bool) {
	event := model.Event{Type: "presence", UserID: id, Online: online}
	for c := range h.clients {
		h.deliver(c, event)
	}
}

func ServeWS(h *Hub, db *store.Postgres, user model.PublicUser, sessionHash string, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	c := &Client{user: user, sessionHash: sessionHash, conn: conn, send: make(chan model.Event, 64), hub: h, store: db}
	go c.writePump()
	h.join <- c
	c.readPump()
}

func (c *Client) readPump() {
	defer func() { c.hub.leave <- c; _ = c.conn.Close() }()
	c.conn.SetReadLimit(4096)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { return c.conn.SetReadDeadline(time.Now().Add(pongWait)) })

	for {
		var incoming struct {
			Type   string `json:"type"`
			Room   string `json:"room"`
			PeerID int64  `json:"peer_id"`
			Text   string `json:"text"`
		}
		if err := c.conn.ReadJSON(&incoming); err != nil {
			return
		}
		// A logout or expired session also revokes an existing WebSocket.
		authCtx, authCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, authErr := c.store.GetUserBySession(authCtx, c.sessionHash)
		authCancel()
		if authErr != nil {
			return
		}
		if incoming.Room != "general" && incoming.Room != "direct" {
			continue
		}
		if incoming.Room == "direct" && (incoming.PeerID <= 0 || incoming.PeerID == c.user.ID) {
			continue
		}

		if incoming.Type == "typing" {
			c.hub.events <- model.Event{Type: "typing", UserID: c.user.ID, Sender: c.user.Username,
				Room: incoming.Room, PeerID: incoming.PeerID}
			continue
		}
		if incoming.Type != "message" {
			continue
		}
		text := strings.TrimSpace(incoming.Text)
		if text == "" || utf8.RuneCountInString(text) > 1000 || len(text) > 4000 {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var m model.Message
		var err error
		if incoming.Room == "direct" {
			m, err = c.store.InsertDirect(ctx, c.user, incoming.PeerID, text)
		} else {
			m, err = c.store.InsertGeneral(ctx, c.user, text)
		}
		cancel()
		if err != nil {
			if !errors.Is(err, store.ErrInvalidPeer) {
				log.Printf("message save error: %v", err)
			}
			select {
			case c.send <- model.Event{Type: "error", Text: "Не удалось отправить сообщение"}:
			default:
			}
			continue
		}
		c.hub.events <- model.Event{Type: "message", Message: &m}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() { ticker.Stop(); _ = c.conn.Close() }()
	for {
		select {
		case event, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteJSON(event); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
