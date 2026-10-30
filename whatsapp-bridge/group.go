package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Group management: create, photo, name, description, permissions and members.
// Every action here is visible to the group members, so the MCP tools are configured to ask before running.

// groupPhotoJPEG turns any JPEG/PNG into what WhatsApp accepts for a group picture: a square JPEG up to 640x640
// (center-cropped, area-averaged down).
func groupPhotoJPEG(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open photo: %v", err)
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("photo must be a JPEG or PNG image: %v", err)
	}
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	x0, y0 := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2
	out := side
	if out > 640 {
		out = 640
	}
	dst := image.NewRGBA(image.Rect(0, 0, out, out))
	for y := 0; y < out; y++ {
		sy0, sy1 := y0+y*side/out, y0+(y+1)*side/out
		if sy1 == sy0 {
			sy1++
		}
		for x := 0; x < out; x++ {
			sx0, sx1 := x0+x*side/out, x0+(x+1)*side/out
			if sx1 == sx0 {
				sx1++
			}
			var r, g, bl, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r, g, bl, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), n+1
				}
			}
			dst.Set(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseGroupJID(chatJID string) (types.JID, error) {
	jid, err := types.ParseJID(strings.TrimSpace(chatJID))
	if err != nil || jid.Server != types.GroupServer {
		return types.JID{}, fmt.Errorf("invalid group JID %q (expected ...@g.us)", chatJID)
	}
	return jid, nil
}

func cleanPhone(p string) string {
	return strings.NewReplacer("+", "", " ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(p))
}

// phoneJIDs validates phone numbers (digits with country code) and returns their WhatsApp JIDs.
func phoneJIDs(client *whatsmeow.Client, phones []string) ([]types.JID, []string, error) {
	var jids []types.JID
	var notOn []string
	for _, p := range phones {
		p = cleanPhone(p)
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return nil, nil, fmt.Errorf("invalid phone %q (use digits with country code, e.g. 5527999999999)", p)
		}
		resp, err := client.IsOnWhatsApp(context.Background(), []string{"+" + p})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to check %s: %v", p, err)
		}
		if len(resp) == 0 || !resp[0].IsIn {
			notOn = append(notOn, p)
			continue
		}
		jids = append(jids, types.NewJID(p, types.DefaultUserServer))
	}
	return jids, notOn, nil
}

// memberJIDs finds current members by phone, returning the JID the group uses for them (phone or LID).
func memberJIDs(info *types.GroupInfo, phones []string) ([]types.JID, []string) {
	var jids []types.JID
	var missing []string
	for _, p := range phones {
		p = cleanPhone(p)
		found := false
		for _, m := range info.Participants {
			if m.JID.User == p || m.PhoneNumber.User == p {
				jids = append(jids, m.JID)
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, p)
		}
	}
	return jids, missing
}

func memberPhone(m types.GroupParticipant) string {
	if !m.PhoneNumber.IsEmpty() {
		return m.PhoneNumber.User
	}
	return m.JID.User
}

// participantErrors explains per-person failures WhatsApp returns when adding/creating.
func participantErrors(ps []types.GroupParticipant) []string {
	var out []string
	for _, p := range ps {
		if p.Error == 0 {
			continue
		}
		why := fmt.Sprintf("erro %d", p.Error)
		switch p.Error {
		case 403:
			why = "a privacidade da pessoa não deixa adicionar; mande o link de convite (get_group_info com include_invite_link)"
		case 408:
			why = "a pessoa saiu do grupo há pouco; mande o link de convite"
		case 409:
			why = "já está no grupo"
		case 401:
			why = "a pessoa bloqueou você"
		case 404:
			why = "número não está no WhatsApp"
		}
		out = append(out, fmt.Sprintf("%s: %s", memberPhone(p), why))
	}
	return out
}

type groupReq struct {
	ChatJID      string   `json:"chat_jid"`
	Name         *string  `json:"name"`
	Description  *string  `json:"description"`
	Participants []string `json:"participants"`
	PhotoPath    string   `json:"photo_path"`
	RemovePhoto  bool     `json:"remove_photo"`
	// Permissions: only_admins_send = "send messages: admins only"; only_admins_edit = "edit group info: admins only";
	// only_admins_add = "add members: admins only"; approve_new_members = "approve new members".
	OnlyAdminsSend    *bool  `json:"only_admins_send"`
	OnlyAdminsEdit    *bool  `json:"only_admins_edit"`
	OnlyAdminsAdd     *bool  `json:"only_admins_add"`
	ApproveNewMembers *bool  `json:"approve_new_members"`
	Action            string `json:"action"`
	IncludeInviteLink bool   `json:"include_invite_link"`
	ResetInviteLink   bool   `json:"reset_invite_link"`
}

// applyGroupSettings changes only the fields that were given; returns what was done and what failed.
func applyGroupSettings(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, req groupReq) ([]string, []string) {
	ctx := context.Background()
	var done, failed []string
	step := func(label string, err error) {
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", label, err))
		} else {
			done = append(done, label)
		}
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		err := client.SetGroupName(ctx, jid, name)
		if err == nil {
			messageStore.db.Exec("UPDATE chats SET name = ? WHERE jid = ?", name, jid.String())
		}
		step("nome", err)
	}
	if req.Description != nil {
		step("descrição", client.SetGroupTopic(ctx, jid, "", "", *req.Description))
	}
	if req.PhotoPath != "" {
		img, err := groupPhotoJPEG(req.PhotoPath)
		if err == nil {
			_, err = client.SetGroupPhoto(ctx, jid, img)
		}
		step("foto", err)
	} else if req.RemovePhoto {
		_, err := client.SetGroupPhoto(ctx, jid, nil)
		step("foto removida", err)
	}
	if req.OnlyAdminsSend != nil {
		step(fmt.Sprintf("só admins enviam mensagens = %v", *req.OnlyAdminsSend), client.SetGroupAnnounce(ctx, jid, *req.OnlyAdminsSend))
	}
	if req.OnlyAdminsEdit != nil {
		step(fmt.Sprintf("só admins editam os dados = %v", *req.OnlyAdminsEdit), client.SetGroupLocked(ctx, jid, *req.OnlyAdminsEdit))
	}
	if req.OnlyAdminsAdd != nil {
		mode := types.GroupMemberAddModeAllMember
		if *req.OnlyAdminsAdd {
			mode = types.GroupMemberAddModeAdmin
		}
		step(fmt.Sprintf("só admins adicionam membros = %v", *req.OnlyAdminsAdd), client.SetGroupMemberAddMode(ctx, jid, mode))
	}
	if req.ApproveNewMembers != nil {
		step(fmt.Sprintf("aprovar novos membros = %v", *req.ApproveNewMembers), client.SetGroupJoinApprovalMode(ctx, jid, *req.ApproveNewMembers))
	}
	return done, failed
}

func createGroup(client *whatsmeow.Client, messageStore *MessageStore, req groupReq) (map[string]any, error) {
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	name := strings.TrimSpace(*req.Name)
	if len([]rune(name)) > 100 {
		return nil, fmt.Errorf("group name is limited to 100 characters")
	}
	if len(req.Participants) == 0 {
		return nil, fmt.Errorf("at least one participant phone is required")
	}
	if req.PhotoPath != "" { // validate before creating, so a bad file doesn't leave a half-made group
		if _, err := groupPhotoJPEG(req.PhotoPath); err != nil {
			return nil, err
		}
	}
	jids, notOn, err := phoneJIDs(client, req.Participants)
	if err != nil {
		return nil, err
	}
	if len(jids) == 0 {
		return nil, fmt.Errorf("none of the numbers is on WhatsApp: %s", strings.Join(notOn, ", "))
	}
	info, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{Name: name, Participants: jids})
	if err != nil {
		return nil, fmt.Errorf("failed to create group: %v", err)
	}
	storeGroup(messageStore, info.JID.String(), info.Name, time.Now(), waLog.Noop)
	settings := req
	settings.Name = nil // already set on creation
	done, failed := applyGroupSettings(client, messageStore, info.JID, settings)
	return map[string]any{
		"success":       true,
		"chat_jid":      info.JID.String(),
		"name":          info.Name,
		"members":       len(info.Participants),
		"not_added":     append(participantErrors(info.Participants), prefixAll(notOn, "não está no WhatsApp: ")...),
		"settings_done": done,
		"settings_fail": failed,
	}, nil
}

func prefixAll(items []string, prefix string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = prefix + s
	}
	return out
}

func updateGroupMembers(client *whatsmeow.Client, jid types.JID, action string, phones []string) (map[string]any, error) {
	ctx := context.Background()
	changes := map[string]whatsmeow.ParticipantChange{
		"add": whatsmeow.ParticipantChangeAdd, "remove": whatsmeow.ParticipantChangeRemove,
		"promote": whatsmeow.ParticipantChangePromote, "demote": whatsmeow.ParticipantChangeDemote,
	}
	change, ok := changes[action]
	if !ok {
		return nil, fmt.Errorf("action must be add, remove, promote or demote")
	}
	if len(phones) == 0 {
		return nil, fmt.Errorf("at least one phone is required")
	}
	var jids []types.JID
	var skipped []string
	if change == whatsmeow.ParticipantChangeAdd {
		var notOn []string
		var err error
		jids, notOn, err = phoneJIDs(client, phones)
		if err != nil {
			return nil, err
		}
		skipped = prefixAll(notOn, "não está no WhatsApp: ")
	} else {
		info, err := client.GetGroupInfo(ctx, jid)
		if err != nil {
			return nil, fmt.Errorf("failed to read group: %v", err)
		}
		var missing []string
		jids, missing = memberJIDs(info, phones)
		skipped = prefixAll(missing, "não é membro do grupo: ")
	}
	if len(jids) == 0 {
		return nil, fmt.Errorf("nothing to do: %s", strings.Join(skipped, "; "))
	}
	res, err := client.UpdateGroupParticipants(ctx, jid, jids, change)
	if err != nil {
		return nil, fmt.Errorf("failed to %s members (are you an admin of this group?): %v", action, err)
	}
	failed := participantErrors(res)
	var okPhones []string
	for _, p := range res {
		if p.Error == 0 {
			okPhones = append(okPhones, memberPhone(p))
		}
	}
	return map[string]any{"success": len(okPhones) > 0, "action": action, "done": okPhones,
		"failed": append(failed, skipped...)}, nil
}

func groupInfo(client *whatsmeow.Client, jid types.JID, inviteLink, resetLink bool) (map[string]any, error) {
	ctx := context.Background()
	info, err := client.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, fmt.Errorf("failed to read group: %v", err)
	}
	me := client.Store.ID.ToNonAD().User
	myLID := client.Store.GetLID().ToNonAD().User
	amAdmin := false
	var members []map[string]any
	for _, p := range info.Participants {
		role := "membro"
		if p.IsSuperAdmin {
			role = "criador"
		} else if p.IsAdmin {
			role = "admin"
		}
		phone := memberPhone(p)
		name := ""
		if c, err := client.Store.Contacts.GetContact(ctx, p.JID); err == nil {
			name = c.FullName
			if name == "" {
				name = c.PushName
			}
		}
		if (phone == me || p.JID.User == myLID) && (p.IsAdmin || p.IsSuperAdmin) {
			amAdmin = true
		}
		members = append(members, map[string]any{"phone": phone, "name": name, "role": role})
	}
	out := map[string]any{
		"chat_jid":            info.JID.String(),
		"name":                info.Name,
		"description":         info.Topic,
		"created":             info.GroupCreated.Format("2006-01-02 15:04"),
		"members_count":       len(info.Participants),
		"members":             members,
		"i_am_admin":          amAdmin,
		"only_admins_send":    info.IsAnnounce,
		"only_admins_edit":    info.IsLocked,
		"only_admins_add":     info.MemberAddMode == types.GroupMemberAddModeAdmin,
		"approve_new_members": info.IsJoinApprovalRequired,
	}
	if inviteLink || resetLink {
		link, err := client.GetGroupInviteLink(ctx, jid, resetLink)
		if err != nil {
			out["invite_link_error"] = err.Error()
		} else {
			out["invite_link"] = link
		}
	}
	return out, nil
}

func registerGroupRoutes(client *whatsmeow.Client, messageStore *MessageStore) {
	reply := func(w http.ResponseWriter, res map[string]any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(res)
	}
	decode := func(w http.ResponseWriter, r *http.Request) (groupReq, bool) {
		var req groupReq
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return req, false
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return req, false
		}
		return req, true
	}

	http.HandleFunc("/api/group/create", func(w http.ResponseWriter, r *http.Request) {
		if req, ok := decode(w, r); ok {
			res, err := createGroup(client, messageStore, req)
			reply(w, res, err)
		}
	})

	http.HandleFunc("/api/group/settings", func(w http.ResponseWriter, r *http.Request) {
		req, ok := decode(w, r)
		if !ok {
			return
		}
		jid, err := parseGroupJID(req.ChatJID)
		if err != nil {
			reply(w, nil, err)
			return
		}
		done, failed := applyGroupSettings(client, messageStore, jid, req)
		if len(done) == 0 && len(failed) == 0 {
			reply(w, nil, fmt.Errorf("nothing to change: pass name, description, photo_path, remove_photo or a permission"))
			return
		}
		reply(w, map[string]any{"success": len(failed) == 0, "done": done, "failed": failed}, nil)
	})

	http.HandleFunc("/api/group/members", func(w http.ResponseWriter, r *http.Request) {
		req, ok := decode(w, r)
		if !ok {
			return
		}
		jid, err := parseGroupJID(req.ChatJID)
		if err != nil {
			reply(w, nil, err)
			return
		}
		res, err := updateGroupMembers(client, jid, req.Action, req.Participants)
		reply(w, res, err)
	})

	http.HandleFunc("/api/group/info", func(w http.ResponseWriter, r *http.Request) {
		req, ok := decode(w, r)
		if !ok {
			return
		}
		jid, err := parseGroupJID(req.ChatJID)
		if err != nil {
			reply(w, nil, err)
			return
		}
		res, err := groupInfo(client, jid, req.IncludeInviteLink, req.ResetInviteLink)
		reply(w, res, err)
	})
}
