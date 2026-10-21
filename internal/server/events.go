package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"septwebclip/internal/store"
)

// event 是推送给浏览器的消息。
type event struct {
	Type   string      `json:"type"` // create | update | delete | presence
	Clip   *store.Clip `json:"clip,omitempty"`
	ID     string      `json:"id,omitempty"`
	Online int         `json:"online,omitempty"`
}

// hub 按墙分组管理 SSE 连接。
type hub struct {
	mu    sync.Mutex
	rooms map[string]map[chan []byte]struct{}
}

func newHub() *hub { return &hub{rooms: map[string]map[chan []byte]struct{}{}} }

func (h *hub) subscribe(room string) chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	subs := h.rooms[room]
	if subs == nil {
		subs = map[chan []byte]struct{}{}
		h.rooms[room] = subs
	}
	subs[ch] = struct{}{}
	n := len(subs)
	h.mu.Unlock()
	h.publish(room, event{Type: "presence", Online: n})
	return ch
}

func (h *hub) unsubscribe(room string, ch chan []byte) {
	h.mu.Lock()
	subs := h.rooms[room]
	delete(subs, ch)
	n := len(subs)
	if n == 0 {
		delete(h.rooms, room)
	}
	h.mu.Unlock()
	if n > 0 {
		h.publish(room, event{Type: "presence", Online: n})
	}
}

func (h *hub) publish(room string, ev event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.rooms[room] {
		select {
		case ch <- b:
		default: // 客户端太慢就丢弃，它重连后会重新拉取全量列表
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, room string) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // 让 nginx 不缓冲这条连接

	ch := s.hub.subscribe(room)
	defer s.hub.unsubscribe(room, ch)

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	if rc.Flush() != nil {
		return
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		if rc.Flush() != nil {
			return
		}
	}
}
