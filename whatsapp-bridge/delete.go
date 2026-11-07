package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waSyncAction "go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Deleting messages, like the phone does:
//   - "everyone": revoke ("Apagar para todos"). Own messages up to ~60 h after sending; in a group where I am admin,
//     other people's messages too. Everyone sees "Mensagem apagada".
//   - "me": "Apagar para mim". Only disappears from my devices (app state regular_high, synced to the phone);
//     nobody else is notified. Works on any message, any age.
// The local store never loses the text: deleted_at / deleted_for_me_at are set and list_messages shows the mark.

const revokeWindow = 60 * time.Hour

type storedMsg struct {
	id, sender, senderJID, content, mediaType string
	fromMe                                    bool
	ts                                        time.Time
}

func loadStoredMsg(messageStore *MessageStore, chat types.JID, id string) (storedMsg, error) {
	m := storedMsg{id: id}
	err := messageStore.db.QueryRow(
		`SELECT COALESCE(sender,''), COALESCE(sender_jid,''), COALESCE(content,''), COALESCE(media_type,''), is_from_me, timestamp
		 FROM messages WHERE id = ? AND chat_jid = ?`, id, chat.String()).
		Scan(&m.sender, &m.senderJID, &m.content, &m.mediaType, &m.fromMe, &m.ts)
	if err != nil {
		return m, fmt.Errorf("mensagem %s não encontrada na conversa %s", id, chat.String())
	}
	return m, nil
}

func amGroupAdmin(client *whatsmeow.Client, chat types.JID) (bool, error) {
	info, err := client.GetGroupInfo(context.Background(), chat)
	if err != nil {
		return false, err
	}
	me := client.Store.ID.ToNonAD().User
	myLID := client.Store.GetLID().ToNonAD().User
	for _, p := range info.Participants {
		if (memberPhone(p) == me || p.JID.User == myLID) && (p.IsAdmin || p.IsSuperAdmin) {
			return true, nil
		}
	}
	return false, nil
}

func revokeMessage(client *whatsmeow.Client, messageStore *MessageStore, chat types.JID, m storedMsg, isAdmin *bool) error {
	if m.fromMe {
		if time.Since(m.ts) > revokeWindow {
			return fmt.Errorf("enviada em %s: passou do prazo de ~60 h do WhatsApp para apagar para todos (use scope \"me\")",
				m.ts.Local().Format("02/01 15:04"))
		}
	} else {
		if chat.Server != types.GroupServer {
			return fmt.Errorf("mensagem de outra pessoa em conversa individual só pode ser apagada para mim (scope \"me\")")
		}
		if isAdmin == nil || !*isAdmin {
			return fmt.Errorf("mensagem de outra pessoa: só admin do grupo apaga para todos (use scope \"me\")")
		}
	}
	sender := types.EmptyJID
	if !m.fromMe {
		author, err := messageAuthor(client, messageStore, chat, m.id)
		if err != nil {
			return err
		}
		sender = author
	}
	if _, err := client.SendMessage(context.Background(), chat, client.BuildRevoke(chat, sender, m.id)); err != nil {
		return fmt.Errorf("falha ao apagar: %v", err)
	}
	messageStore.db.Exec("UPDATE messages SET deleted_at = ? WHERE id = ? AND chat_jid = ? AND deleted_at IS NULL",
		time.Now(), m.id, chat.String())
	return nil
}

// appStateKey gives the fromMe flag and participant used in message-level app state indexes (star, delete for me):
// participant is "0" except for other people's messages in groups.
func appStateKey(client *whatsmeow.Client, messageStore *MessageStore, chat types.JID, m storedMsg) (string, string, error) {
	if m.fromMe {
		return "1", "0", nil
	}
	if chat.Server == types.GroupServer {
		author, err := messageAuthor(client, messageStore, chat, m.id)
		if err != nil {
			return "", "", err
		}
		return "0", author.String(), nil
	}
	return "0", "0", nil
}

func buildDeleteForMe(client *whatsmeow.Client, messageStore *MessageStore, chat types.JID, m storedMsg) (appstate.MutationInfo, error) {
	fromMe, senderJID, err := appStateKey(client, messageStore, chat, m)
	if err != nil {
		return appstate.MutationInfo{}, err
	}
	return appstate.MutationInfo{
		Index:   []string{appstate.IndexDeleteMessageForMe, chat.String(), m.id, fromMe, senderJID},
		Version: 3,
		Value: &waSyncAction.SyncActionValue{
			DeleteMessageForMeAction: &waSyncAction.DeleteMessageForMeAction{
				DeleteMedia:      proto.Bool(true),
				MessageTimestamp: proto.Int64(m.ts.Unix()),
			},
		},
	}, nil
}

func sendAppStateRetry(client *whatsmeow.Client, patch appstate.PatchInfo) error {
	ctx := context.Background()
	err := client.SendAppState(ctx, patch)
	if err != nil {
		// 409 / mismatching LTHash: local copy of regular_high is stale. Re-fetch it fully and try once more.
		if ferr := client.FetchAppState(ctx, patch.Type, true, false); ferr == nil {
			err = client.SendAppState(ctx, patch)
		}
	}
	return err
}

// deleteMessages deletes several messages of one chat. Returns a per-message report.
func deleteMessages(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, ids []string, scope string) (bool, string) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return false, fmt.Sprintf("chat_jid inválido: %v", err)
	}
	if scope != "everyone" && scope != "me" {
		return false, `scope deve ser "everyone" (apagar para todos) ou "me" (apagar só para mim)`
	}
	var ok, failed []string
	var isAdmin *bool
	var forMe []appstate.MutationInfo
	var forMeIDs []string
	for i, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		m, err := loadStoredMsg(messageStore, chat, id)
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		if scope == "me" {
			mut, err := buildDeleteForMe(client, messageStore, chat, m)
			if err != nil {
				failed = append(failed, id+": "+err.Error())
				continue
			}
			forMe = append(forMe, mut)
			forMeIDs = append(forMeIDs, id)
			continue
		}
		if !m.fromMe && isAdmin == nil && chat.Server == types.GroupServer {
			a, err := amGroupAdmin(client, chat)
			if err != nil {
				failed = append(failed, id+": não consegui ler o grupo para saber se sou admin: "+err.Error())
				continue
			}
			isAdmin = &a
		}
		if i > 0 {
			time.Sleep(time.Duration(1500+rand.Intn(2500)) * time.Millisecond) // a person deletes one at a time
		}
		if err := revokeMessage(client, messageStore, chat, m, isAdmin); err != nil {
			failed = append(failed, id+": "+err.Error())
			continue
		}
		ok = append(ok, id)
	}
	// "Me" deletions go as app state patches, in chunks (the phone's "select several > delete for me" does the same).
	for start := 0; start < len(forMe); start += 50 {
		end := min(start+50, len(forMe))
		patch := appstate.PatchInfo{Type: appstate.WAPatchRegularHigh, Mutations: forMe[start:end]}
		if err := sendAppStateRetry(client, patch); err != nil {
			for _, id := range forMeIDs[start:end] {
				failed = append(failed, id+": falha ao apagar para mim: "+err.Error())
			}
			continue
		}
		now := time.Now()
		for _, id := range forMeIDs[start:end] {
			messageStore.db.Exec("UPDATE messages SET deleted_for_me_at = ? WHERE id = ? AND chat_jid = ?", now, id, chat.String())
			ok = append(ok, id)
		}
		if end < len(forMe) {
			time.Sleep(time.Duration(1000+rand.Intn(2000)) * time.Millisecond)
		}
	}
	what := "para todos"
	if scope == "me" {
		what = "só para mim"
	}
	msg := fmt.Sprintf("Apagadas %s: %d de %d.", what, len(ok), len(ok)+len(failed))
	if len(failed) > 0 {
		msg += " Falharam: " + strings.Join(failed, "; ")
	}
	return len(ok) > 0, msg
}

// handleDeleteForMe records "delete for me" done on the phone (or another device), keeping the content.
func handleDeleteForMe(messageStore *MessageStore, v *events.DeleteForMe) {
	ts := v.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	messageStore.db.Exec("UPDATE messages SET deleted_for_me_at = ? WHERE id = ? AND chat_jid = ? AND deleted_for_me_at IS NULL",
		ts, v.MessageID, v.ChatJID.String())
}

func registerDeleteRoutes(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID    string   `json:"chat_jid"`
			MessageIDs []string `json:"message_ids"`
			Scope      string   `json:"scope"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || len(req.MessageIDs) == 0 {
			http.Error(w, "chat_jid and message_ids are required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ok, msg := deleteMessages(client, messageStore, req.ChatJID, req.MessageIDs, req.Scope)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})
}
