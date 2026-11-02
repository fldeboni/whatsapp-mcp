package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Shared contacts (vCards) and locations: rendered as readable text so list_messages shows them, and sending
// contacts as real WhatsApp contact cards.

const liveLocationPrefix = "[LOCALIZAÇÃO EM TEMPO REAL]"

func mapsLink(lat, lng float64) string {
	return fmt.Sprintf("https://maps.google.com/?q=%.6f,%.6f", lat, lng)
}

// vcardSummary keeps the useful vCard fields (name, phones with WhatsApp id, e-mails, company, title, address,
// site, note) in one line each, dropping photos and encoding noise.
func vcardSummary(displayName, vcard string) string {
	var lines []string
	name := displayName
	for _, raw := range strings.Split(strings.ReplaceAll(vcard, "\r\n", "\n"), "\n") {
		key, val, ok := strings.Cut(raw, ":")
		if !ok || val == "" {
			continue
		}
		field := strings.ToUpper(key)
		if i := strings.Index(field, ";"); i >= 0 {
			field = field[:i]
		}
		if j := strings.LastIndex(field, "."); j >= 0 { // item1.TEL
			field = field[j+1:]
		}
		val = strings.TrimSpace(strings.ReplaceAll(val, "\\,", ","))
		switch field {
		case "FN":
			if name == "" {
				name = val
			}
		case "TEL":
			phone := val
			if _, waid, found := strings.Cut(strings.ToLower(key), "waid="); found {
				if f := strings.FieldsFunc(waid, func(r rune) bool { return r == ';' || r == ':' }); len(f) > 0 {
					phone = fmt.Sprintf("%s (WhatsApp %s)", val, f[0])
				}
			}
			lines = append(lines, "telefone: "+phone)
		case "EMAIL":
			lines = append(lines, "e-mail: "+val)
		case "ORG":
			lines = append(lines, "empresa: "+strings.Trim(strings.ReplaceAll(val, ";", " "), " "))
		case "TITLE":
			lines = append(lines, "cargo: "+val)
		case "ADR":
			lines = append(lines, "endereço: "+strings.Join(strings.Fields(strings.ReplaceAll(val, ";", " ")), " "))
		case "URL":
			lines = append(lines, "site: "+val)
		case "NOTE":
			lines = append(lines, "obs: "+val)
		}
	}
	return fmt.Sprintf("[CONTATO] %s\n%s", name, strings.Join(lines, "\n"))
}

// extractSharedContent renders contact cards and locations; "" when the message is neither.
func extractSharedContent(msg *waProto.Message) string {
	if c := msg.GetContactMessage(); c != nil {
		return vcardSummary(c.GetDisplayName(), c.GetVcard())
	}
	if ca := msg.GetContactsArrayMessage(); ca != nil {
		var parts []string
		for _, c := range ca.GetContacts() {
			parts = append(parts, vcardSummary(c.GetDisplayName(), c.GetVcard()))
		}
		return strings.Join(parts, "\n")
	}
	if l := msg.GetLocationMessage(); l != nil {
		var b strings.Builder
		b.WriteString("[LOCALIZAÇÃO]")
		for _, s := range []string{l.GetName(), l.GetAddress()} {
			if s != "" {
				b.WriteString(" " + s)
			}
		}
		fmt.Fprintf(&b, " — %.6f, %.6f — %s", l.GetDegreesLatitude(), l.GetDegreesLongitude(),
			mapsLink(l.GetDegreesLatitude(), l.GetDegreesLongitude()))
		if l.GetURL() != "" {
			b.WriteString("\nsite: " + l.GetURL())
		}
		if l.GetComment() != "" {
			b.WriteString("\n" + l.GetComment())
		}
		return b.String()
	}
	if l := msg.GetLiveLocationMessage(); l != nil {
		s := fmt.Sprintf("%s %.6f, %.6f — %s", liveLocationPrefix, l.GetDegreesLatitude(), l.GetDegreesLongitude(),
			mapsLink(l.GetDegreesLatitude(), l.GetDegreesLongitude()))
		if l.GetAccuracyInMeters() > 0 {
			s += fmt.Sprintf(" (precisão %d m)", l.GetAccuracyInMeters())
		}
		if l.GetSpeedInMps() > 0 {
			s += fmt.Sprintf(" (%.0f km/h)", l.GetSpeedInMps()*3.6)
		}
		if l.GetCaption() != "" {
			s += "\n" + l.GetCaption()
		}
		return s
	}
	return ""
}

// updateLiveLocation folds a live-location update into the sender's open live-location message (instead of a new
// row every few seconds). Returns true when it updated an existing row.
func updateLiveLocation(messageStore *MessageStore, id, chatJID, sender, content string, ts time.Time) bool {
	var openID string
	err := messageStore.db.QueryRow(`SELECT id FROM messages WHERE chat_jid = ? AND sender = ? AND content LIKE ?
		AND timestamp >= ? ORDER BY timestamp DESC LIMIT 1`,
		chatJID, sender, liveLocationPrefix+"%", ts.Add(-12*time.Hour)).Scan(&openID)
	if err != nil || openID == id {
		return false
	}
	content += fmt.Sprintf("\n(posição atualizada em %s)", ts.Local().Format("2006-01-02 15:04:05"))
	messageStore.db.Exec("UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ?", content, openID, chatJID)
	return true
}

type sharedContact struct {
	Name    string   `json:"name"`
	Phones  []string `json:"phones"`
	Emails  []string `json:"emails"`
	Company string   `json:"company"`
	Title   string   `json:"title"`
	Note    string   `json:"note"`
}

func vcardEscape(s string) string {
	return strings.NewReplacer("\\", "\\\\", ",", "\\,", ";", "\\;", "\n", "\\n").Replace(strings.TrimSpace(s))
}

// buildVCard makes a vCard 3.0 that WhatsApp shows as a contact card with "Message"/"Add" buttons
// (the waid parameter links each phone to its WhatsApp account).
func buildVCard(c sharedContact) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\nVERSION:3.0\n")
	fmt.Fprintf(&b, "N:;%s;;;\nFN:%s\n", vcardEscape(c.Name), vcardEscape(c.Name))
	if c.Company != "" {
		fmt.Fprintf(&b, "ORG:%s\n", vcardEscape(c.Company))
	}
	if c.Title != "" {
		fmt.Fprintf(&b, "TITLE:%s\n", vcardEscape(c.Title))
	}
	for _, p := range c.Phones {
		digits := cleanPhone(p)
		fmt.Fprintf(&b, "TEL;type=CELL;type=VOICE;waid=%s:+%s\n", digits, digits)
	}
	for _, e := range c.Emails {
		fmt.Fprintf(&b, "EMAIL;type=INTERNET:%s\n", strings.TrimSpace(e))
	}
	if c.Note != "" {
		fmt.Fprintf(&b, "NOTE:%s\n", vcardEscape(c.Note))
	}
	b.WriteString("END:VCARD")
	return b.String()
}

func sendContacts(client *whatsmeow.Client, recipient string, contacts []sharedContact, reply *waProto.ContextInfo) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}
	if len(contacts) == 0 {
		return false, "at least one contact is required"
	}
	for i, c := range contacts {
		if strings.TrimSpace(c.Name) == "" || len(c.Phones) == 0 {
			return false, fmt.Sprintf("contact %d needs a name and at least one phone", i+1)
		}
		for _, p := range c.Phones {
			if d := cleanPhone(p); d == "" || strings.Trim(d, "0123456789") != "" {
				return false, fmt.Sprintf("invalid phone %q (digits with country code)", p)
			}
		}
	}
	var to types.JID
	if strings.Contains(recipient, "@") {
		var err error
		if to, err = types.ParseJID(recipient); err != nil {
			return false, fmt.Sprintf("invalid recipient: %v", err)
		}
	} else {
		to = types.NewJID(cleanPhone(recipient), types.DefaultUserServer)
	}
	msg := &waProto.Message{}
	if len(contacts) == 1 {
		msg.ContactMessage = &waProto.ContactMessage{DisplayName: proto.String(contacts[0].Name),
			Vcard: proto.String(buildVCard(contacts[0])), ContextInfo: reply}
	} else {
		var cards []*waProto.ContactMessage
		for _, c := range contacts {
			cards = append(cards, &waProto.ContactMessage{DisplayName: proto.String(c.Name), Vcard: proto.String(buildVCard(c))})
		}
		msg.ContactsArrayMessage = &waProto.ContactsArrayMessage{
			DisplayName: proto.String(fmt.Sprintf("%d contatos", len(contacts))), Contacts: cards, ContextInfo: reply}
	}
	if _, err := client.SendMessage(context.Background(), to, msg); err != nil {
		return false, fmt.Sprintf("failed to send contact: %v", err)
	}
	var names []string
	for _, c := range contacts {
		names = append(names, c.Name)
	}
	return true, fmt.Sprintf("Contact card sent to %s: %s", recipient, strings.Join(names, ", "))
}

func registerShareRoutes(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/send_contact", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Recipient string          `json:"recipient"`
			Contacts  []sharedContact `json:"contacts"`
			ReplyTo   string          `json:"reply_to"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Recipient == "" {
			http.Error(w, "recipient and contacts are required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var reply *waProto.ContextInfo
		if req.ReplyTo != "" {
			var err error
			if reply, err = buildReplyContext(client, messageStore, req.Recipient, req.ReplyTo); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: err.Error()})
				return
			}
		}
		ok, msg := sendContacts(client, req.Recipient, req.Contacts, reply)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})
}
