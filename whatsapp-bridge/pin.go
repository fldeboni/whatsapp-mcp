package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Pinning ("Fixar") a message in a chat, like the phone: everyone in the chat sees it in the bar on top, for 24 h,
// 7 or 30 days. WhatsApp keeps up to 3 pinned per chat (a 4th replaces the oldest). In groups where only admins can
// edit the group info, only admins can pin. pinned_until in the store mirrors it (also pins made by others/the phone).

const defaultPinSecs = 7 * 24 * 3600

func pinDuration(s string) (uint32, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "24h", "1d", "24 horas", "1 dia":
		return 24 * 3600, nil
	case "", "7d", "7 dias":
		return defaultPinSecs, nil
	case "30d", "30 dias":
		return 30 * 24 * 3600, nil
	}
	return 0, fmt.Errorf("duração inválida %q: use \"24h\", \"7d\" ou \"30d\"", s)
}

func pinMessage(client *whatsmeow.Client, messageStore *MessageStore, chatJID, id string, pin bool, duration string) (bool, string) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return false, fmt.Sprintf("chat_jid inválido: %v", err)
	}
	secs, err := pinDuration(duration)
	if err != nil {
		return false, err.Error()
	}
	id = strings.TrimSpace(id)
	m, err := loadStoredMsg(messageStore, chat, id)
	if err != nil {
		return false, err.Error()
	}
	if chat.Server == types.GroupServer {
		if info, err := client.GetGroupInfo(context.Background(), chat); err == nil && info.IsLocked {
			if admin, _ := amGroupAdmin(client, chat); !admin {
				return false, "Neste grupo só admins podem fixar mensagens (e você não é admin)"
			}
		}
	}
	sender := types.EmptyJID
	if !m.fromMe {
		if sender, err = messageAuthor(client, messageStore, chat, id); err != nil {
			return false, err.Error()
		}
	}
	typ := waProto.PinInChatMessage_PIN_FOR_ALL
	if !pin {
		typ = waProto.PinInChatMessage_UNPIN_FOR_ALL
	}
	msg := &waProto.Message{
		PinInChatMessage: &waProto.PinInChatMessage{
			Key:               client.BuildMessageKey(chat, sender, id),
			Type:              typ.Enum(),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
		},
	}
	if pin {
		msg.MessageContextInfo = &waProto.MessageContextInfo{MessageAddOnDurationInSecs: proto.Uint32(secs)}
	}
	if _, err := client.SendMessage(context.Background(), chat, msg); err != nil {
		return false, fmt.Sprintf("Falha ao %s: %v", map[bool]string{true: "fixar", false: "desafixar"}[pin], err)
	}
	if !pin {
		setPinned(messageStore, chat.String(), id, time.Time{})
		return true, "Mensagem desafixada."
	}
	until := time.Now().Add(time.Duration(secs) * time.Second)
	setPinned(messageStore, chat.String(), id, until)
	return true, fmt.Sprintf("Mensagem fixada até %s (o WhatsApp mantém no máximo 3 fixadas por conversa; a 4ª substitui a mais antiga).",
		until.Local().Format("02/01/2006 15:04"))
}

// setPinned: zero time = unpinned.
func setPinned(messageStore *MessageStore, chatJID, id string, until time.Time) {
	if until.IsZero() {
		messageStore.db.Exec("UPDATE messages SET pinned_until = NULL WHERE id = ? AND chat_jid = ?", id, chatJID)
		return
	}
	// UTC, whole seconds: "2026-11-14 13:00:00+00:00", easy to parse on the Python side.
	messageStore.db.Exec("UPDATE messages SET pinned_until = ? WHERE id = ? AND chat_jid = ?",
		until.UTC().Truncate(time.Second), id, chatJID)
}

// handlePinMessage mirrors pins made by anyone in the chat (or by me on the phone). Returns true if it was a pin.
func handlePinMessage(messageStore *MessageStore, chatJID string, m *waProto.Message, ts time.Time) bool {
	p := m.GetPinInChatMessage()
	if p == nil {
		return false
	}
	id := p.GetKey().GetID()
	if id == "" {
		return true
	}
	if p.GetType() != waProto.PinInChatMessage_PIN_FOR_ALL {
		setPinned(messageStore, chatJID, id, time.Time{})
		return true
	}
	secs := m.GetMessageContextInfo().GetMessageAddOnDurationInSecs()
	if secs == 0 {
		secs = defaultPinSecs
	}
	if ms := p.GetSenderTimestampMS(); ms > 0 {
		ts = time.UnixMilli(ms)
	}
	setPinned(messageStore, chatJID, id, ts.Add(time.Duration(secs)*time.Second))
	return true
}

func registerPinRoutes(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/pin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID   string `json:"chat_jid"`
			MessageID string `json:"message_id"`
			Pin       *bool  `json:"pin"`
			Duration  string `json:"duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || req.MessageID == "" {
			http.Error(w, "chat_jid and message_id are required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ok, msg := pinMessage(client, messageStore, req.ChatJID, req.MessageID, req.Pin == nil || *req.Pin, req.Duration)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})
}
