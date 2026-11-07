package main

import (
	"encoding/json"
	"fmt"
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

// Starring ("Favoritar") messages: app state regular_high, synced to the phone's "Mensagens favoritas".
// starred_at in the store mirrors it (also when starred/unstarred on the phone).

func starMessages(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, ids []string, starred bool) (bool, string) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return false, fmt.Sprintf("chat_jid inválido: %v", err)
	}
	var muts []appstate.MutationInfo
	var mutIDs, failed []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		m, err := loadStoredMsg(messageStore, chat, id)
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		fromMe, sender, err := appStateKey(client, messageStore, chat, m)
		if err != nil {
			failed = append(failed, id+": "+err.Error())
			continue
		}
		muts = append(muts, appstate.MutationInfo{
			Index:   []string{appstate.IndexStar, chat.String(), id, fromMe, sender},
			Version: 2,
			Value:   &waSyncAction.SyncActionValue{StarAction: &waSyncAction.StarAction{Starred: proto.Bool(starred)}},
		})
		mutIDs = append(mutIDs, id)
	}
	ok := 0
	for start := 0; start < len(muts); start += 50 {
		end := min(start+50, len(muts))
		if err := sendAppStateRetry(client, appstate.PatchInfo{Type: appstate.WAPatchRegularHigh, Mutations: muts[start:end]}); err != nil {
			for _, id := range mutIDs[start:end] {
				failed = append(failed, id+": "+err.Error())
			}
			continue
		}
		for _, id := range mutIDs[start:end] {
			setStarred(messageStore, chat.String(), id, starred, time.Now())
			ok++
		}
	}
	what := "Favoritadas"
	if !starred {
		what = "Desfavoritadas"
	}
	msg := fmt.Sprintf("%s: %d de %d.", what, ok, ok+len(failed))
	if len(failed) > 0 {
		msg += " Falharam: " + strings.Join(failed, "; ")
	}
	return ok > 0, msg
}

func setStarred(messageStore *MessageStore, chatJID, id string, starred bool, ts time.Time) {
	if starred {
		messageStore.db.Exec("UPDATE messages SET starred_at = ? WHERE id = ? AND chat_jid = ?", ts, id, chatJID)
	} else {
		messageStore.db.Exec("UPDATE messages SET starred_at = NULL WHERE id = ? AND chat_jid = ?", id, chatJID)
	}
}

// handleStar mirrors stars set on the phone or another device.
func handleStar(messageStore *MessageStore, v *events.Star) {
	ts := v.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	setStarred(messageStore, v.ChatJID.String(), v.MessageID, v.Action.GetStarred(), ts)
}

func registerStarRoutes(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/star", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID    string   `json:"chat_jid"`
			MessageIDs []string `json:"message_ids"`
			Starred    *bool    `json:"starred"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || len(req.MessageIDs) == 0 {
			http.Error(w, "chat_jid and message_ids are required", http.StatusBadRequest)
			return
		}
		starred := req.Starred == nil || *req.Starred
		w.Header().Set("Content-Type", "application/json")
		ok, msg := starMessages(client, messageStore, req.ChatJID, req.MessageIDs, starred)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})
}
