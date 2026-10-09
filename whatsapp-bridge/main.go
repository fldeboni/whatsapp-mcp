package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"bytes"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// storeDir returns where session keys, messages and media live.
// Defaults to %USERPROFILE%\.whatsapp-mcp\store: outside synced folders (OneDrive) and outside AppData, which
// Windows virtualizes for packaged apps like Claude desktop (files written there by Claude are invisible to
// Explorer/logon, so a Startup shortcut could not find the bridge).
func storeDir() string {
	if dir := os.Getenv("WHATSAPP_STORE_DIR"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".whatsapp-mcp", "store")
	}
	return "store"
}

// storeDSN builds a sqlite DSN for a file inside storeDir.
func storeDSN(name string) string {
	return "file:" + filepath.ToSlash(filepath.Join(storeDir(), name)) + "?_foreign_keys=on"
}

// safeFilename strips any path components from a sender-controlled file name,
// so a document named "..\..\Startup\x.bat" cannot be written outside the chat folder.
func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "file_" + time.Now().Format("20060102_150405")
	}
	return name
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir(), 0700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", storeDSN("messages.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Full sender JID (user@lid / user@s.whatsapp.net): `sender` only keeps the user part,
	// which is not enough to send read receipts. Ignore the error when the column already exists.
	db.Exec(`ALTER TABLE messages ADD COLUMN sender_jid TEXT`)
	db.Exec(`CREATE TABLE IF NOT EXISTS read_marks (chat_jid TEXT PRIMARY KEY, marked_until TIMESTAMP)`)

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, senderJID, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, sender_jid, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, senderJID, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// Media captions (deal groups post almost everything as image + caption)
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}

	// Interactive messages (e.g. the "Copiar chave Pix" card): body text + native-flow buttons.
	if im := msg.GetInteractiveMessage(); im != nil {
		return extractInteractiveText(im)
	}
	return ""
}

// extractInteractiveText renders an interactive message as text. Native-flow buttons carry their data as JSON
// (a Pix key card has the key, its type and the merchant name); known Pix fields become a readable line and the
// raw params are kept (trimmed) so nothing is lost when WhatsApp changes the format.
func extractInteractiveText(im *waProto.InteractiveMessage) string {
	var parts []string
	if t := im.GetHeader().GetTitle(); t != "" {
		parts = append(parts, t)
	}
	if t := im.GetBody().GetText(); t != "" {
		parts = append(parts, t)
	}
	if t := im.GetFooter().GetText(); t != "" {
		parts = append(parts, t)
	}
	for _, b := range im.GetNativeFlowMessage().GetButtons() {
		raw := b.GetButtonParamsJSON()
		var params any
		if json.Unmarshal([]byte(raw), &params) == nil {
			if pix := findPixKey(params); pix != "" {
				parts = append(parts, pix)
			}
		}
		if len(raw) > 600 {
			raw = raw[:600] + "..."
		}
		parts = append(parts, fmt.Sprintf("[botão %s] %s", b.GetName(), raw))
	}
	return strings.Join(parts, "\n")
}

// findPixKey walks button params looking for an object with a Pix key ("key" plus "key_type" or a merchant name).
func findPixKey(v any) string {
	switch t := v.(type) {
	case map[string]any:
		key, _ := t["key"].(string)
		keyType, _ := t["key_type"].(string)
		name, _ := t["merchant_name"].(string)
		if key != "" && (keyType != "" || name != "") {
			s := "Chave Pix"
			if keyType != "" {
				s += " (" + keyType + ")"
			}
			s += ": " + key
			if name != "" {
				s += " — " + name
			}
			return s
		}
		for _, child := range t {
			if s := findPixKey(child); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range t {
			if s := findPixKey(child); s != "" {
				return s
			}
		}
	}
	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"` // message ID to quote (reply), must be stored in this chat
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string, reply *waProto.ContextInfo) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		case "zip":
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/zip"
		case "pdf":
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/pdf"
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		fmt.Println("Media uploaded", resp)

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			// Base name only: the full local path (user name, folders) must not leak to the recipient.
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(filepath.Base(mediaPath)),
				FileName:      proto.String(filepath.Base(mediaPath)),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else if text, mentioned := buildMentions(client, recipientJID, message); len(mentioned) > 0 || reply != nil {
		// "@<phone>" in a group only becomes a real mention (name highlighted, person notified) when the
		// message carries the mentioned JIDs; plain text just shows the number. A reply carries the quoted message.
		ctxInfo := reply
		if ctxInfo == nil {
			ctxInfo = &waProto.ContextInfo{}
		}
		ctxInfo.MentionedJID = mentioned
		msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: ctxInfo,
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	_, err = client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
// normalizeChat maps a DM addressed by LID (xxx@lid) to the phone-number JID, so the same person
// is always one chat (findable by phone/contact name) no matter how WhatsApp addressed the message.
func normalizeChat(client *whatsmeow.Client, jid types.JID) types.JID {
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if pn, err := client.Store.LIDs.GetPNForLID(context.Background(), jid); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return jid
}

func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	msg.Info.Chat = normalizeChat(client, msg.Info.Chat)
	chatJID := msg.Info.Chat.String()
	// Display sender by phone number (resolves to contact names); sender_jid keeps the real JID for receipts.
	sender := normalizeChat(client, msg.Info.Sender).User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		msg.Info.Sender.ToNonAD().String(),
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	chatDir := filepath.Join(storeDir(), safeFilename(strings.ReplaceAll(chatJID, ":", "_")))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// One folder per message: the "already downloaded" shortcut below must only reuse THIS message's file.
	// Keyed by filename alone, a corrected file re-sent with the same name returned the old copy.
	chatDir = filepath.Join(chatDir, safeFilename(messageID))
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file
	filename = safeFilename(filename)
	localPath = filepath.Join(chatDir, filename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// groupSenderServer tells whether a group addresses participants by LID or phone number,
// needed to rebuild full sender JIDs for rows stored before sender_jid existed.
var groupSenderServer = map[string]string{}

func senderServerFor(client *whatsmeow.Client, chat types.JID) string {
	if s, ok := groupSenderServer[chat.String()]; ok {
		return s
	}
	server := types.DefaultUserServer
	if info, err := client.GetGroupInfo(context.Background(), chat); err == nil && info.AddressingMode == types.AddressingModeLID {
		server = types.HiddenUserServer
	}
	groupSenderServer[chat.String()] = server
	return server
}

// requestChatHistory asks the phone for up to `count` messages older than the oldest one stored for the chat.
func requestChatHistory(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, count int) (bool, string) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return false, fmt.Sprintf("invalid chat JID: %v", err)
	}
	var id, senderJID string
	var ts time.Time
	var fromMe bool
	err = messageStore.db.QueryRow(
		"SELECT id, timestamp, is_from_me, COALESCE(sender_jid, '') FROM messages WHERE chat_jid = ? ORDER BY timestamp ASC LIMIT 1",
		chatJID).Scan(&id, &ts, &fromMe, &senderJID)
	if err != nil {
		return false, "no stored message in this chat to anchor the history request"
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: fromMe},
		ID:            id,
		Timestamp:     ts,
	}
	if senderJID != "" {
		info.Sender, _ = types.ParseJID(senderJID)
	}
	if _, err := client.SendPeerMessage(context.Background(), client.BuildHistorySyncRequest(info, count)); err != nil {
		return false, fmt.Sprintf("failed to request history: %v", err)
	}
	return true, fmt.Sprintf("Requested up to %d messages older than %s from the phone; they arrive in a few seconds (phone must be online)",
		count, ts.Format("2006-01-02 15:04"))
}

// saveContact creates or updates a WhatsApp contact (the "contact" app-state action WhatsApp Web/app use since 2024,
// synced to all devices; saveOnPhone also asks the phone to save it in its own address book).
func saveContact(client *whatsmeow.Client, messageStore *MessageStore, phone, fullName, firstName string, saveOnPhone bool) (bool, string) {
	ctx := context.Background()
	phone = strings.TrimLeft(strings.TrimSpace(phone), "+")
	if phone == "" || strings.Trim(phone, "0123456789") != "" || fullName == "" {
		return false, "phone (digits with country code) and full_name are required"
	}
	if firstName == "" {
		firstName = strings.Fields(fullName)[0]
	}
	resp, err := client.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return false, fmt.Sprintf("failed to check number: %v", err)
	}
	if len(resp) == 0 || !resp[0].IsIn {
		return false, fmt.Sprintf("%s is not on WhatsApp", phone)
	}
	// Contacts are keyed by the PHONE-number JID; the LID goes in its own field. IsOnWhatsApp may answer with the
	// LID, so never use its JID as the key (the phone wouldn't attach the name to the number).
	pn := types.NewJID(phone, types.DefaultUserServer)
	lid := types.JID{}
	if resp[0].JID.Server == types.HiddenUserServer {
		lid = resp[0].JID.ToNonAD()
	} else if l, err := client.Store.LIDs.GetLIDForPN(ctx, pn); err == nil {
		lid = l.ToNonAD()
	}
	action := &waSyncAction.ContactAction{
		FullName:                 proto.String(fullName),
		FirstName:                proto.String(firstName),
		PnJID:                    proto.String(pn.String()),
		SaveOnPrimaryAddressbook: proto.Bool(saveOnPhone),
	}
	if !lid.IsEmpty() {
		action.LidJID = proto.String(lid.String())
	}
	patch := appstate.PatchInfo{
		Type: appstate.WAPatchCriticalUnblockLow, // the contact list collection
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexContact, pn.String()},
			Version: 2,
			Value:   &waSyncAction.SyncActionValue{ContactAction: action},
		}},
	}
	if err := client.SendAppState(ctx, patch); err != nil {
		return false, fmt.Sprintf("failed to save contact: %v", err)
	}
	// Reflect it locally right away (search_contacts / chat names) without waiting for the app-state echo.
	client.Store.Contacts.PutContactName(ctx, pn, fullName, firstName) // signature: (jid, fullName, firstName)
	messageStore.db.Exec("UPDATE chats SET name = ? WHERE jid = ?", fullName, pn.String())
	return true, fmt.Sprintf("Contact saved: %s (%s)", fullName, pn.User)
}

// messageAuthor returns who wrote a stored message, as WhatsApp expects it in message keys (reactions, edits):
// our own JID for our messages, the sender's JID otherwise (rebuilt from the group's addressing mode for old rows).
func messageAuthor(client *whatsmeow.Client, messageStore *MessageStore, chat types.JID, messageID string) (types.JID, error) {
	var sender, senderJID string
	var fromMe bool
	err := messageStore.db.QueryRow(
		"SELECT COALESCE(sender,''), COALESCE(sender_jid,''), is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chat.String()).Scan(&sender, &senderJID, &fromMe)
	if err != nil {
		return types.JID{}, fmt.Errorf("message %s not found in chat %s", messageID, chat.String())
	}
	switch {
	case fromMe:
		return client.Store.ID.ToNonAD(), nil
	case senderJID != "":
		j, err := types.ParseJID(senderJID)
		return j.ToNonAD(), err
	case chat.Server == types.GroupServer && sender != "":
		return types.NewJID(strings.Split(sender, "@")[0], senderServerFor(client, chat)), nil
	default:
		return chat, nil // direct chat: the other party
	}
}

// sendReaction reacts to a stored message with an emoji; an empty emoji removes our reaction.
func sendReaction(client *whatsmeow.Client, messageStore *MessageStore, chatJID, messageID, emoji string) (bool, string) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return false, fmt.Sprintf("invalid chat JID: %v", err)
	}
	author, err := messageAuthor(client, messageStore, chat, messageID)
	if err != nil {
		return false, err.Error()
	}
	if _, err := client.SendMessage(context.Background(), chat, client.BuildReaction(chat, author, messageID, emoji)); err != nil {
		return false, fmt.Sprintf("failed to send reaction: %v", err)
	}
	if emoji == "" {
		return true, fmt.Sprintf("Reaction removed from %s", messageID)
	}
	return true, fmt.Sprintf("Reacted %s to %s", emoji, messageID)
}

// buildReplyContext quotes a stored message (WhatsApp "reply"): the recipient sees the quoted bubble above the text.
func buildReplyContext(client *whatsmeow.Client, messageStore *MessageStore, recipient, messageID string) (*waProto.ContextInfo, error) {
	chatJID := recipient
	if !strings.Contains(chatJID, "@") {
		chatJID += "@" + types.DefaultUserServer
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return nil, fmt.Errorf("invalid recipient: %v", err)
	}
	var content, mediaType, filename, senderJID string
	var fromMe bool
	err = messageStore.db.QueryRow(
		`SELECT COALESCE(content,''), COALESCE(media_type,''), COALESCE(filename,''), COALESCE(sender_jid,''), is_from_me
		 FROM messages WHERE id = ? AND chat_jid = ?`, messageID, chat.String()).Scan(&content, &mediaType, &filename, &senderJID, &fromMe)
	if err != nil {
		return nil, fmt.Errorf("reply_to %s not found in chat %s", messageID, chat.String())
	}
	// Who wrote the quoted message: needed in groups so WhatsApp can render/jump to it.
	var participant types.JID
	if fromMe {
		participant = client.Store.ID.ToNonAD()
		if chat.Server == types.GroupServer && senderServerFor(client, chat) == types.HiddenUserServer {
			participant = client.Store.GetLID().ToNonAD()
		}
	} else if senderJID != "" {
		participant, _ = types.ParseJID(senderJID)
		participant = participant.ToNonAD()
	}
	quoted := &waProto.Message{}
	switch mediaType {
	case "document":
		quoted.DocumentMessage = &waProto.DocumentMessage{FileName: proto.String(filename), Title: proto.String(filename), Caption: proto.String(content)}
	case "image":
		quoted.ImageMessage = &waProto.ImageMessage{Caption: proto.String(content)}
	case "video":
		quoted.VideoMessage = &waProto.VideoMessage{Caption: proto.String(content)}
	case "audio":
		quoted.AudioMessage = &waProto.AudioMessage{}
	default:
		quoted.Conversation = proto.String(content)
	}
	ctxInfo := &waProto.ContextInfo{StanzaID: proto.String(messageID), QuotedMessage: quoted}
	if !participant.IsEmpty() {
		ctxInfo.Participant = proto.String(participant.String())
	}
	return ctxInfo, nil
}

var mentionPattern = regexp.MustCompile(`@(\d{8,15})\b`)

// buildMentions turns "@<phone>" tokens of a group message into real mentions. Groups addressed by LID need the
// mention (and the "@..." in the text) to use the participant's LID, otherwise WhatsApp shows the raw number.
func buildMentions(client *whatsmeow.Client, chat types.JID, text string) (string, []string) {
	if chat.Server != types.GroupServer {
		return text, nil
	}
	useLID := senderServerFor(client, chat) == types.HiddenUserServer
	var mentioned []string
	seen := map[string]bool{}
	out := mentionPattern.ReplaceAllStringFunc(text, func(tok string) string {
		pn := types.NewJID(tok[1:], types.DefaultUserServer)
		target := pn
		if useLID {
			if lid, err := client.Store.LIDs.GetLIDForPN(context.Background(), pn); err == nil && !lid.IsEmpty() {
				target = lid.ToNonAD()
			}
		}
		if !seen[target.String()] {
			seen[target.String()] = true
			mentioned = append(mentioned, target.String())
		}
		return "@" + target.User
	})
	return out, mentioned
}

// storeGroup inserts a group chat or refreshes its name, never touching last_message_time of an existing row.
func storeGroup(messageStore *MessageStore, jid, name string, ts time.Time, logger waLog.Logger) {
	if name == "" {
		return
	}
	res, err := messageStore.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		 ON CONFLICT(jid) DO UPDATE SET name = excluded.name WHERE chats.name IS NOT excluded.name`,
		jid, name, ts)
	if err != nil {
		logger.Warnf("Failed to store group %s: %v", jid, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logger.Infof("Group stored/renamed: %s (%s)", name, jid)
	}
}

func groupTime(g *types.GroupInfo) time.Time {
	if !g.GroupCreated.IsZero() {
		return g.GroupCreated
	}
	if !g.NameSetAt.IsZero() {
		return g.NameSetAt
	}
	return time.Now()
}

func syncJoinedGroups(client *whatsmeow.Client, messageStore *MessageStore, logger waLog.Logger) {
	groups, err := client.GetJoinedGroups(context.Background())
	if err != nil {
		logger.Warnf("Failed to list joined groups: %v", err)
		return
	}
	for _, g := range groups {
		storeGroup(messageStore, g.JID.String(), g.Name, groupTime(g), logger)
	}
	logger.Infof("Joined groups synced: %d", len(groups))
}

// unmarkedSince is the cutoff for "incoming messages not yet marked as read": the last mark, at most 3 days back.
func unmarkedSince(messageStore *MessageStore, chatJID string) time.Time {
	since := time.Now().Add(-72 * time.Hour)
	var marked time.Time
	if messageStore.db.QueryRow("SELECT marked_until FROM read_marks WHERE chat_jid = ?", chatJID).Scan(&marked) == nil && marked.After(since) {
		since = marked
	}
	return since
}

func pendingIncoming(messageStore *MessageStore, chatJID string) int {
	var n int
	messageStore.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ? AND is_from_me = 0 AND timestamp > ?",
		chatJID, unmarkedSince(messageStore, chatJID)).Scan(&n)
	return n
}

// ---- Auto mark-as-read -----------------------------------------------------------------------
// Config: <data dir>\auto-markread.json (next to the store folder), re-read on every cycle, so edits
// apply without restarting. Missing file or "enabled": false = feature off.

type autoMarkReadConfig struct {
	Enabled     bool     `json:"enabled"`
	WindowStart string   `json:"window_start"`         // "07:00"
	WindowEnd   string   `json:"window_end"`           // "22:00"
	MinMinutes  int      `json:"interval_min_minutes"` // 30
	MaxMinutes  int      `json:"interval_max_minutes"` // 60
	Chats       []string `json:"chats"`                // chat JIDs (groups end with @g.us)
}

func autoMarkReadConfigPath() string {
	return filepath.Join(filepath.Dir(storeDir()), "auto-markread.json")
}

func loadAutoMarkRead() (*autoMarkReadConfig, error) {
	cfg := &autoMarkReadConfig{WindowStart: "07:00", WindowEnd: "22:00", MinMinutes: 30, MaxMinutes: 60}
	data, err := os.ReadFile(autoMarkReadConfigPath())
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.MinMinutes < 5 {
		cfg.MinMinutes = 5
	}
	if cfg.MaxMinutes < cfg.MinMinutes {
		cfg.MaxMinutes = cfg.MinMinutes
	}
	return cfg, nil
}

// atClock returns today's date at "HH:MM" local time.
func atClock(day time.Time, hm string) time.Time {
	var h, m int
	fmt.Sscanf(hm, "%d:%d", &h, &m)
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, time.Local)
}

func randMinutes(min, max int) time.Duration {
	return time.Duration(min*60+rand.Intn((max-min)*60+1)) * time.Second
}

// nextAutoRun picks the next run time: now + random interval, pushed into the allowed window.
func nextAutoRun(cfg *autoMarkReadConfig, from time.Time) time.Time {
	next := from.Add(randMinutes(cfg.MinMinutes, cfg.MaxMinutes))
	start, end := atClock(next, cfg.WindowStart), atClock(next, cfg.WindowEnd)
	switch {
	case next.Before(start):
		return start.Add(randMinutes(0, 20))
	case !next.Before(end):
		return atClock(next.AddDate(0, 0, 1), cfg.WindowStart).Add(randMinutes(0, 20))
	}
	return next
}

func runAutoMarkRead(client *whatsmeow.Client, messageStore *MessageStore, cfg *autoMarkReadConfig, logger waLog.Logger) {
	chats := append([]string(nil), cfg.Chats...)
	rand.Shuffle(len(chats), func(i, j int) { chats[i], chats[j] = chats[j], chats[i] })
	done := 0
	for _, chatJID := range chats {
		if !client.IsConnected() {
			logger.Warnf("auto mark-read: not connected, stopping this cycle")
			return
		}
		if pendingIncoming(messageStore, chatJID) == 0 {
			continue // nothing new: don't touch the chat at all
		}
		if done > 0 {
			// Same human pacing as the MCP tool: 4-12s between chats, sometimes a longer pause.
			if rand.Float64() < 0.15 {
				time.Sleep(time.Duration(15+rand.Intn(16)) * time.Second)
			} else {
				time.Sleep(time.Duration(4+rand.Intn(9)) * time.Second)
			}
		}
		ok, msg := markChatAsRead(client, messageStore, chatJID)
		if ok {
			done++
		}
		logger.Infof("auto mark-read: %s", msg)
	}
	logger.Infof("auto mark-read: cycle done, %d chats marked", done)
}

// startAutoMarkRead runs forever. It polls the wall clock every minute instead of sleeping until the
// target: on Windows Go's monotonic timers don't advance while the laptop sleeps/hibernates.
func startAutoMarkRead(client *whatsmeow.Client, messageStore *MessageStore, logger waLog.Logger) {
	go func() {
		next := time.Now().Add(randMinutes(1, 5)) // never a burst right at startup
		for {
			time.Sleep(time.Minute)
			now := time.Now().Round(0) // strip monotonic reading: compare wall clock
			if now.Before(next) {
				continue
			}
			cfg, err := loadAutoMarkRead()
			if err != nil || !cfg.Enabled || len(cfg.Chats) == 0 {
				next = now.Add(10 * time.Minute)
				continue
			}
			if start, end := atClock(now, cfg.WindowStart), atClock(now, cfg.WindowEnd); now.Before(start) || !now.Before(end) {
				next = nextAutoRun(cfg, now)
				logger.Infof("auto mark-read: outside %s-%s, next run %s", cfg.WindowStart, cfg.WindowEnd, next.Format("02/01 15:04"))
				continue
			}
			runAutoMarkRead(client, messageStore, cfg, logger)
			next = nextAutoRun(cfg, time.Now().Round(0))
			logger.Infof("auto mark-read: next run %s", next.Format("02/01 15:04"))
		}
	}()
}

// markChatAsRead does what WhatsApp Web does when you open a chat: read receipts for the incoming
// messages (one receipt per sender), then the "mark chat as read" app state patch keyed on the last message.
// The phone only clears the unread badge with the receipts; the patch alone (timestamp only) is ignored.
func markChatAsRead(client *whatsmeow.Client, messageStore *MessageStore, chatJID string) (bool, string) {
	ctx := context.Background()
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return false, fmt.Sprintf("invalid chat JID: %v", err)
	}
	isGroup := chat.Server == types.GroupServer

	// Incoming messages not yet marked: since the last mark, at most 3 days back, newest 300.
	since := unmarkedSince(messageStore, chatJID)
	rows, err := messageStore.db.Query(
		`SELECT id, sender, COALESCE(sender_jid, ''), timestamp FROM messages
		 WHERE chat_jid = ? AND is_from_me = 0 AND timestamp > ? ORDER BY timestamp DESC LIMIT 300`,
		chatJID, since)
	if err != nil {
		return false, fmt.Sprintf("failed to query messages: %v", err)
	}
	type pending struct {
		ids    []types.MessageID
		sender types.JID
	}
	bySender := map[string]*pending{}
	var order []string
	var lastID, lastSender string
	var lastTS time.Time
	for rows.Next() {
		var id, sender, senderJID string
		var ts time.Time
		if rows.Scan(&id, &sender, &senderJID, &ts) != nil {
			continue
		}
		if lastID == "" {
			lastID, lastTS = id, ts
		}
		var sjid types.JID
		if isGroup {
			if senderJID == "" && strings.Contains(sender, "@") {
				senderJID = sender
			}
			if senderJID != "" {
				sjid, _ = types.ParseJID(senderJID)
			} else if sender != "" {
				sjid = types.NewJID(sender, senderServerFor(client, chat))
			}
			if sjid.IsEmpty() {
				continue
			}
		}
		if lastSender == "" && !sjid.IsEmpty() {
			lastSender = sjid.String()
		}
		key := sjid.String()
		if bySender[key] == nil {
			bySender[key] = &pending{sender: sjid}
			order = append(order, key)
		}
		bySender[key].ids = append(bySender[key].ids, id)
	}
	rows.Close()

	receipts, total := 0, 0
	for _, key := range order {
		p := bySender[key]
		total += len(p.ids)
		for start := 0; start < len(p.ids); start += 50 {
			end := start + 50
			if end > len(p.ids) {
				end = len(p.ids)
			}
			if err := client.MarkRead(ctx, p.ids[start:end], time.Now(), chat, p.sender); err != nil {
				return false, fmt.Sprintf("failed to send read receipt in %s: %v", chatJID, err)
			}
			receipts++
			// Web sends these as a quick burst when a chat is opened; a small gap keeps it from being instant.
			time.Sleep(time.Duration(150+rand.Intn(350)) * time.Millisecond)
		}
	}

	if lastTS.IsZero() {
		messageStore.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", chatJID).Scan(&lastTS)
	}
	var lastKey *waCommon.MessageKey
	if lastID != "" {
		lastKey = &waCommon.MessageKey{RemoteJID: proto.String(chatJID), FromMe: proto.Bool(false), ID: proto.String(lastID)}
		if isGroup && lastSender != "" {
			lastKey.Participant = proto.String(lastSender)
		}
	}
	// Best effort: the receipts are what clear the badge on the phone. The app state patch can fail with
	// 409/"mismatching LTHash" when the local regular_low state is out of sync, which must not fail the call.
	note := ""
	if err := client.SendAppState(ctx, appstate.BuildMarkChatAsRead(chat, true, lastTS, lastKey)); err != nil {
		fmt.Printf("markread %s: app state patch skipped: %v\n", chatJID, err)
		note = ", app state skipped"
	}
	if receipts == 0 && note != "" {
		return false, fmt.Sprintf("nothing to mark in %s (no stored incoming messages) and app state failed", chatJID)
	}

	messageStore.db.Exec("INSERT OR REPLACE INTO read_marks (chat_jid, marked_until) VALUES (?, ?)", chatJID, time.Now())
	return true, fmt.Sprintf("Marked %s as read (%d messages, %d receipts%s)", chatJID, total, receipts, note)
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	// Keep the query string: WhatsApp's direct_path is "/v/t62...enc?ccb=..&oh=..&oe=..", and the CDN answers
	// 403 without the oh/oe auth parameters (upstream stripped them, so no document ever downloaded).
	return "/" + parts[1]
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int) {
	// Handler for sending messages
	http.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Println("Received request to send message", req.Message, req.MediaPath)

		// Send the message
		var reply *waProto.ContextInfo
		if req.ReplyTo != "" {
			var err error
			if reply, err = buildReplyContext(client, messageStore, req.Recipient, req.ReplyTo); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: err.Error()})
				return
			}
		}
		success, message := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath, reply)
		fmt.Println("Message sent", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	})

	// Handler for downloading media
	http.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Handler for marking a chat as read (same as "Mark as read" in the chat list; syncs to the phone)
	http.HandleFunc("/api/markread", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ChatJID string `json:"chat_jid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
			http.Error(w, "chat_jid is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		success, message := markChatAsRead(client, messageStore, req.ChatJID)
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: success, Message: message})
	})

	// Handler for creating/updating a contact
	http.HandleFunc("/api/contact", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Phone       string `json:"phone"`
			FullName    string `json:"full_name"`
			FirstName   string `json:"first_name"`
			SaveOnPhone *bool  `json:"save_on_phone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		saveOnPhone := req.SaveOnPhone == nil || *req.SaveOnPhone
		w.Header().Set("Content-Type", "application/json")
		ok, msg := saveContact(client, messageStore, req.Phone, req.FullName, req.FirstName, saveOnPhone)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})

	// Handler for reacting to a message with an emoji ("" removes the reaction)
	http.HandleFunc("/api/react", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID   string `json:"chat_jid"`
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || req.MessageID == "" {
			http.Error(w, "chat_jid and message_id are required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ok, msg := sendReaction(client, messageStore, req.ChatJID, req.MessageID, req.Emoji)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})

	// Handler for fetching older history of one chat from the phone (on-demand history sync).
	// The phone answers asynchronously with a HistorySync event, stored by handleHistorySync.
	http.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID string `json:"chat_jid"`
			Count   int    `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
			http.Error(w, "chat_jid is required", http.StatusBadRequest)
			return
		}
		if req.Count <= 0 || req.Count > 50 {
			req.Count = 50
		}
		w.Header().Set("Content-Type", "application/json")
		ok, msg := requestChatHistory(client, messageStore, req.ChatJID, req.Count)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: ok, Message: msg})
	})

	// Handler for asking the phone to resend a message that failed to decrypt
	http.HandleFunc("/api/rerequest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID   string `json:"chat_jid"`
			SenderJID string `json:"sender_jid"`
			ID        string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || req.SenderJID == "" || req.ID == "" {
			http.Error(w, "chat_jid, sender_jid and id are required", http.StatusBadRequest)
			return
		}
		chat, err1 := types.ParseJID(req.ChatJID)
		sender, err2 := types.ParseJID(req.SenderJID)
		w.Header().Set("Content-Type", "application/json")
		if err1 != nil || err2 != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: "invalid JID"})
			return
		}
		_, err := client.SendPeerMessage(context.Background(), client.BuildUnavailableMessageRequest(chat, sender.ToNonAD(), req.ID))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: err.Error()})
			return
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: true, Message: "requested from phone"})
	})

	// Start the server
	// Loopback only: the API has no auth, so it must never be reachable from the network.
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServe(serverAddr, nil); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir(), 0700); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}
	logger.Infof("Store directory: %s", storeDir())

	container, err := sqlstore.New(context.Background(), "sqlite3", storeDSN("whatsapp.db"), dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// How this device shows up in the phone's "Linked devices" list. Only sent at pairing time
	// (existing sessions keep their old name/icon until re-paired). Upstream sent UNKNOWN -> "?" icon, "Outro dispositivo".
	wastore.SetOSInfo("Claude (WhatsApp MCP)", [3]uint32{1, 0, 0})
	wastore.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}
	// Messages that fail to decrypt (typical after the laptop sleeps: "no sender key") are re-requested
	// from the phone instead of being silently lost.
	client.AutomaticMessageRerequestFromPhone = true

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			// After sleep/hibernation the regular_low app state (read/archive/mute) can fail with
			// "mismatching LTHash" on every notification; a full re-fetch resets the local hash state.
			go func() {
				if err := client.FetchAppState(context.Background(), appstate.WAPatchRegularLow, true, false); err != nil {
					logger.Warnf("Full app state resync failed: %v", err)
				} else {
					logger.Infof("App state regular_low resynced")
				}
			}()
			// Chats only got a row when a message arrived, so groups without messages (e.g. just created) were
			// invisible to the MCP. Register every joined group and refresh renamed ones on each (re)connect.
			go syncJoinedGroups(client, messageStore, logger)

		case *events.JoinedGroup:
			// Group created by/with you: register it now, it may not get a message for a while.
			storeGroup(messageStore, v.JID.String(), v.Name, groupTime(&v.GroupInfo), logger)

		case *events.GroupInfo:
			if v.Name != nil && v.Name.Name != "" {
				storeGroup(messageStore, v.JID.String(), v.Name.Name, v.Timestamp, logger)
			}

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	startRESTServer(client, messageStore, 8080)

	// Periodic mark-as-read of the chats listed in auto-markread.json (off when the file is missing)
	startAutoMarkRead(client, messageStore, logger)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	// A name that is just the number (or LID) means the contact wasn't known when the chat was first stored
	// (the address book syncs after the first messages): look it up again instead of keeping the number forever.
	if err == nil && existingName != "" && (existingName == jid.User || strings.Trim(existingName, "0123456789") == "") && jid.Server != "g.us" {
		existingName = ""
	}
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if err == nil && contact.FirstName != "" {
			name = contact.FirstName
		} else if err == nil && contact.BusinessName != "" {
			name = contact.BusinessName
		} else if err == nil && contact.PushName != "" {
			name = contact.PushName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}
		jid = normalizeChat(client, jid)
		chatJID = jid.String()

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender, senderJID string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
						senderJID = sender
					} else if isFromMe {
						sender = client.Store.ID.User
						senderJID = client.Store.ID.ToNonAD().String()
					} else {
						sender = jid.User
						senderJID = jid.ToNonAD().String()
					}
				} else {
					sender = jid.User
					senderJID = jid.ToNonAD().String()
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					senderJID,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rand.Seed(int64(duration))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rand.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
