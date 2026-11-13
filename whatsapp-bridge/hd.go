package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// HD photos/videos, like the phone's "HD" option: WhatsApp does a "dual upload". First an SD copy goes as a normal
// message flagged SD_*_PARENT (what old phones and slow connections show), then the original goes wrapped in
// associatedChildMessage, flagged HD_*_CHILD and pointing at the parent. The recipient sees one bubble with the "HD"
// badge and gets the original when opening it. Only worth it when the original is bigger than SD (photo > 1280 px on
// the long side, video > 480p); otherwise a normal send is identical.

const (
	sdImageLongSide  = 1280
	sdVideoShortSide = 480
)

type sdCopy struct {
	data          []byte
	width, height uint32
	thumb         []byte
	seconds       uint32
}

// makeSDImage returns an SD JPEG of the photo, or nil when the original is already SD-sized (no HD needed).
func makeSDImage(path string) (*sdCopy, uint32, uint32, []byte) {
	ffmpeg, ffprobe := findFFTool("ffmpeg"), findFFTool("ffprobe")
	if ffmpeg == "" || ffprobe == "" {
		return nil, 0, 0, nil
	}
	p, err := probeVideo(ffprobe, path)
	if err != nil {
		return nil, 0, 0, nil
	}
	_, _, _, w, h, _ := p.dims()
	thumb := imageThumbnail(ffmpeg, path)
	if w <= sdImageLongSide && h <= sdImageLongSide {
		return nil, uint32(w), uint32(h), thumb
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-frames:v", "1",
		"-vf", fmt.Sprintf("scale='if(gt(iw,ih),%d,-2)':'if(gt(iw,ih),-2,%d)'", sdImageLongSide, sdImageLongSide),
		"-q:v", "3", "-f", "mjpeg", "pipe:1").Output()
	if err != nil || len(out) == 0 {
		return nil, uint32(w), uint32(h), thumb
	}
	sw, sh := scaleDims(w, h, sdImageLongSide, true)
	return &sdCopy{data: out, width: sw, height: sh, thumb: thumb}, uint32(w), uint32(h), thumb
}

// makeSDVideo returns a 480p copy of a (already prepared) video, or nil when it is not above 480p.
func makeSDVideo(hd []byte, info videoInfo) *sdCopy {
	ffmpeg := findFFTool("ffmpeg")
	short := info.Width
	if info.Height < short {
		short = info.Height
	}
	if ffmpeg == "" || short <= sdVideoShortSide {
		return nil
	}
	in, err := os.CreateTemp("", "wa-hd-*.mp4")
	if err != nil {
		return nil
	}
	in.Write(hd)
	in.Close()
	defer os.Remove(in.Name())
	out := strings.TrimSuffix(in.Name(), ".mp4") + "-sd.mp4"
	defer os.Remove(out)
	cmd := exec.Command(ffmpeg, "-y", "-v", "error", "-i", in.Name(), "-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-pix_fmt", "yuv420p",
		"-vf", fmt.Sprintf("scale='if(gt(iw,ih),-2,%d)':'if(gt(iw,ih),%d,-2)'", sdVideoShortSide, sdVideoShortSide),
		"-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", out)
	if err := cmd.Run(); err != nil {
		return nil
	}
	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 || len(data) >= len(hd) {
		return nil
	}
	sw, sh := scaleDims(int(info.Width), int(info.Height), sdVideoShortSide, false)
	return &sdCopy{data: data, width: sw, height: sh, thumb: info.Thumbnail, seconds: info.Seconds}
}

// scaleDims mirrors the ffmpeg scale filters above (byLong: fit the long side; else fit the short side). Even sizes.
func scaleDims(w, h, target int, byLong bool) (uint32, uint32) {
	if w == 0 || h == 0 {
		return 0, 0
	}
	landscape := w > h
	var nw, nh int
	if landscape == byLong {
		nw, nh = target, h*target/w
	} else {
		nh, nw = target, w*target/h
	}
	return uint32(nw &^ 1), uint32(nh &^ 1)
}

func imageThumbnail(ffmpeg, path string) []byte {
	out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-frames:v", "1",
		"-vf", "scale='if(gt(iw,ih),200,-2)':'if(gt(iw,ih),-2,200)'", "-q:v", "6", "-f", "mjpeg", "pipe:1").Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

func withPaired(ctx *waProto.ContextInfo, t waProto.ContextInfo_PairedMediaType) *waProto.ContextInfo {
	if ctx == nil {
		ctx = &waProto.ContextInfo{}
	} else {
		ctx = proto.Clone(ctx).(*waProto.ContextInfo)
	}
	ctx.PairedMediaType = t.Enum()
	return ctx
}

func mediaImage(up whatsmeow.UploadResponse, mime, caption string, ctx *waProto.ContextInfo, w, h uint32, thumb []byte) *waProto.ImageMessage {
	m := &waProto.ImageMessage{
		Mimetype: proto.String(mime), ContextInfo: ctx, JPEGThumbnail: thumb,
		URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
	}
	if caption != "" {
		m.Caption = proto.String(caption)
	}
	if w > 0 && h > 0 {
		m.Width, m.Height = proto.Uint32(w), proto.Uint32(h)
	}
	return m
}

func mediaVideo(up whatsmeow.UploadResponse, caption string, ctx *waProto.ContextInfo, w, h, secs uint32, thumb []byte) *waProto.VideoMessage {
	m := &waProto.VideoMessage{
		Mimetype: proto.String("video/mp4"), ContextInfo: ctx, JPEGThumbnail: thumb,
		URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
	}
	if caption != "" {
		m.Caption = proto.String(caption)
	}
	if w > 0 && h > 0 {
		m.Width, m.Height = proto.Uint32(w), proto.Uint32(h)
	}
	if secs > 0 {
		m.Seconds = proto.Uint32(secs)
	}
	return m
}

// sendHDPair sends SD parent + HD child. ok=false (without having sent anything) means: fall back to a normal send.
func sendHDPair(client *whatsmeow.Client, chat types.JID, mediaType whatsmeow.MediaType, mime string, hd []byte,
	sd *sdCopy, hdW, hdH uint32, hdThumb []byte, vinfo videoInfo, caption string, ctx *waProto.ContextInfo) (bool, string) {
	bg := context.Background()
	sdUp, err := client.Upload(bg, sd.data, mediaType)
	if err != nil {
		return false, ""
	}
	hdUp, err := client.Upload(bg, hd, mediaType)
	if err != nil {
		return false, ""
	}
	parent := &waProto.Message{}
	child := &waProto.Message{}
	assoc := waProto.MessageAssociation_HD_IMAGE_DUAL_UPLOAD
	if mediaType == whatsmeow.MediaVideo {
		assoc = waProto.MessageAssociation_HD_VIDEO_DUAL_UPLOAD
		parent.VideoMessage = mediaVideo(sdUp, caption, withPaired(ctx, waProto.ContextInfo_SD_VIDEO_PARENT), sd.width, sd.height, sd.seconds, sd.thumb)
		child.VideoMessage = mediaVideo(hdUp, "", withPaired(nil, waProto.ContextInfo_HD_VIDEO_CHILD), vinfo.Width, vinfo.Height, vinfo.Seconds, vinfo.Thumbnail)
	} else {
		parent.ImageMessage = mediaImage(sdUp, "image/jpeg", caption, withPaired(ctx, waProto.ContextInfo_SD_IMAGE_PARENT), sd.width, sd.height, sd.thumb)
		child.ImageMessage = mediaImage(hdUp, mime, "", withPaired(nil, waProto.ContextInfo_HD_IMAGE_CHILD), hdW, hdH, hdThumb)
	}
	resp, err := client.SendMessage(bg, chat, parent)
	if err != nil {
		return false, ""
	}
	storeSent(client, chat, parent, resp)
	// Same layout as the phone: the association goes on the OUTER message, next to associatedChildMessage.
	wrapped := &waProto.Message{
		AssociatedChildMessage: &waProto.FutureProofMessage{Message: child},
		MessageContextInfo: &waProto.MessageContextInfo{
			MessageAssociation: &waProto.MessageAssociation{
				AssociationType:  assoc.Enum(),
				ParentMessageKey: client.BuildMessageKey(chat, types.EmptyJID, resp.ID),
			},
		},
	}
	if _, err := client.SendMessage(bg, chat, wrapped); err != nil {
		return true, fmt.Sprintf(" (foi em qualidade normal: a versão HD falhou: %v; ID %s)", err, resp.ID)
	}
	handleHDChild(sentStore, chat.String(), wrapped)
	return true, fmt.Sprintf(" (HD: %.1f MB, prévia SD %.1f MB; ID %s)", float64(len(hd))/(1<<20), float64(len(sd.data))/(1<<20), resp.ID)
}

// handleHDChild: an HD original arrived (ours or someone's) for an SD message already stored. Point the stored row to
// the HD media so download_media gets the original. Returns true when the message was an HD child.
func handleHDChild(messageStore *MessageStore, chatJID string, m *waProto.Message) bool {
	inner := m.GetAssociatedChildMessage().GetMessage()
	if inner == nil {
		return false
	}
	assoc := m.GetMessageContextInfo().GetMessageAssociation() // where the phone puts it
	if assoc == nil {
		assoc = inner.GetMessageContextInfo().GetMessageAssociation()
	}
	t := assoc.GetAssociationType()
	if t != waProto.MessageAssociation_HD_IMAGE_DUAL_UPLOAD && t != waProto.MessageAssociation_HD_VIDEO_DUAL_UPLOAD {
		if b, err := protojson.Marshal(m); err == nil {
			fmt.Printf("associatedChildMessage (not HD) in %s: %.600s\n", chatJID, b)
		}
		return false
	}
	parentID := assoc.GetParentMessageKey().GetID()
	_, _, url, key, sha, encSha, length := extractMediaInfo(inner)
	if parentID != "" && url != "" {
		messageStore.db.Exec(`UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ?
			WHERE id = ? AND chat_jid = ?`, url, key, sha, encSha, length, parentID, chatJID)
	}
	return true
}
