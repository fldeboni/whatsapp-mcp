package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Videos are sent with duration, size and a thumbnail (without them WhatsApp shows a grey box until the video is
// opened), and converted to H.264/AAC MP4 when they would not play everywhere (HEVC from iPhone, AVI, MKV...) or
// are too big. Needs ffmpeg/ffprobe; without them the video goes as is, like before.

const maxVideoBytes = 60 << 20 // WhatsApp refuses large videos; above this we compress

type videoInfo struct {
	Seconds   uint32
	Width     uint32
	Height    uint32
	Thumbnail []byte
}

// findFFTool looks for ffmpeg/ffprobe in PATH and in the winget install folders (the bridge is started by a
// shortcut and may not see a PATH updated after login).
func findFFTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	if p := filepath.Join(local, "Microsoft", "WinGet", "Links", name+".exe"); fileExists(p) {
		return p
	}
	matches, _ := filepath.Glob(filepath.Join(local, "Microsoft", "WinGet", "Packages", "*FFmpeg*", "*", "bin", name+".exe"))
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

type probeResult struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		PixFmt    string `json:"pix_fmt"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Tags      struct {
			Rotate string `json:"rotate"`
		} `json:"tags"`
		SideData []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func probeVideo(ffprobe, path string) (*probeResult, error) {
	out, err := exec.Command(ffprobe, "-v", "error", "-print_format", "json", "-show_streams", "-show_format", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %v", err)
	}
	var r probeResult
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// dims returns codec names and the displayed size (width/height swapped when the phone recorded it rotated).
func (r *probeResult) dims() (vcodec, pixfmt, acodec string, w, h int, seconds float64) {
	for _, s := range r.Streams {
		switch s.CodecType {
		case "video":
			if vcodec != "" {
				continue
			}
			vcodec, pixfmt, w, h = s.CodecName, s.PixFmt, s.Width, s.Height
			rot, _ := strconv.Atoi(s.Tags.Rotate)
			for _, sd := range s.SideData {
				if sd.Rotation != 0 {
					rot = int(sd.Rotation)
				}
			}
			if rot%180 != 0 {
				w, h = h, w
			}
		case "audio":
			if acodec == "" {
				acodec = s.CodecName
			}
		}
	}
	seconds, _ = strconv.ParseFloat(r.Format.Duration, 64)
	return
}

// prepareVideo returns the bytes to upload, the metadata for the message and a note for the user (conversion done,
// or why metadata is missing). On any ffmpeg problem it falls back to the original file.
func prepareVideo(path string, data []byte) ([]byte, videoInfo, string) {
	ffmpeg, ffprobe := findFFTool("ffmpeg"), findFFTool("ffprobe")
	if ffmpeg == "" || ffprobe == "" {
		return data, videoInfo{}, " (ffmpeg não instalado: vídeo enviado sem miniatura nem conversão)"
	}
	probe, err := probeVideo(ffprobe, path)
	if err != nil {
		return data, videoInfo{}, " (não consegui ler o vídeo com ffprobe: enviado como está)"
	}
	vcodec, pixfmt, acodec, w, h, secs := probe.dims()
	if vcodec == "" {
		return data, videoInfo{}, " (arquivo sem trilha de vídeo: enviado como está)"
	}

	src, note := path, ""
	ext := strings.ToLower(filepath.Ext(path))
	compatible := vcodec == "h264" && (pixfmt == "yuv420p" || pixfmt == "yuvj420p") &&
		(acodec == "" || acodec == "aac") && (ext == ".mp4" || ext == ".m4v")
	if !compatible || len(data) > maxVideoBytes {
		tmp, err := os.CreateTemp("", "wa-video-*.mp4")
		if err != nil {
			return data, videoInfo{}, ""
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		args := []string{"-y", "-v", "error", "-i", path, "-map", "0:v:0", "-map", "0:a:0?",
			"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart"}
		if len(data) > maxVideoBytes {
			// Fit the target size: bitrate from duration, and at most 1280 px on the long side.
			kbps := 2500
			if secs > 0 {
				kbps = int(float64(maxVideoBytes)*8*0.9/secs/1000) - 128
			}
			if kbps < 300 {
				kbps = 300
			}
			args = append(args, "-b:v", fmt.Sprintf("%dk", kbps), "-maxrate", fmt.Sprintf("%dk", kbps*3/2),
				"-bufsize", fmt.Sprintf("%dk", kbps*2),
				"-vf", "scale='if(gt(iw,ih),min(1280,iw),-2)':'if(gt(iw,ih),-2,min(1280,ih))'")
		} else {
			args = append(args, "-crf", "23")
		}
		args = append(args, tmp.Name())
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			return data, videoInfo{}, fmt.Sprintf(" (conversão falhou, enviado original: %s)", strings.TrimSpace(string(out)))
		}
		conv, err := os.ReadFile(tmp.Name())
		if err != nil || len(conv) == 0 {
			return data, videoInfo{}, " (conversão falhou, enviado original)"
		}
		if len(conv) > 95<<20 {
			return data, videoInfo{}, " (vídeo grande demais mesmo comprimido)"
		}
		note = fmt.Sprintf(" (convertido de %s/%s para MP4 H.264, %.1f MB → %.1f MB)", vcodec, ext,
			float64(len(data))/(1<<20), float64(len(conv))/(1<<20))
		data = conv
		// Re-read the size from the converted file (scaling may have changed it); the temp file is removed by the
		// defer only after the thumbnail below is taken from it.
		if p, err := probeVideo(ffprobe, tmp.Name()); err == nil {
			_, _, _, w, h, secs = p.dims()
		}
		src = tmp.Name()
	}

	return data, videoInfo{Seconds: uint32(secs + 0.5), Width: uint32(w), Height: uint32(h),
		Thumbnail: videoThumbnail(ffmpeg, src, secs)}, note
}

// videoThumbnail grabs a small JPEG frame (WhatsApp shows it blurred until the video is downloaded).
func videoThumbnail(ffmpeg, path string, secs float64) []byte {
	at := "1"
	if secs > 0 && secs < 2 {
		at = "0"
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-ss", at, "-i", path, "-frames:v", "1",
		"-vf", "scale='if(gt(iw,ih),200,-2)':'if(gt(iw,ih),-2,200)'", "-q:v", "6", "-f", "mjpeg", "pipe:1").Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}
