package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestScaleDims(t *testing.T) {
	if w, h := scaleDims(4000, 3000, 1600, true); w != 1600 || h != 1200 {
		t.Fatalf("landscape long: %dx%d", w, h)
	}
	if w, h := scaleDims(3000, 4000, 1600, true); w != 1200 || h != 1600 {
		t.Fatalf("portrait long: %dx%d", w, h)
	}
	if w, h := scaleDims(1080, 1920, 480, false); w != 480 || h != 852 {
		t.Fatalf("portrait short: %dx%d", w, h)
	}
}

// Needs ffmpeg; skipped otherwise.
func TestMakeSD(t *testing.T) {
	ffmpeg := findFFTool("ffmpeg")
	if ffmpeg == "" {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "big.jpg")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=3000x2000", "-frames:v", "1", img).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	sd, w, h, thumb := makeSDImage(img)
	if sd == nil || w != 3000 || h != 2000 || len(thumb) == 0 || sd.width != 1280 {
		t.Fatalf("image: sd=%v w=%d h=%d", sd != nil, w, h)
	}
	small := filepath.Join(dir, "small.png")
	exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=800x600", "-frames:v", "1", small).Run()
	if sd, _, _, _ := makeSDImage(small); sd != nil {
		t.Fatal("small image should not get an SD copy")
	}

	vid := filepath.Join(dir, "v.mp4")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=1080x1920:rate=30", "-t", "3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", vid).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	data, _ := os.ReadFile(vid)
	hd, info, _ := prepareVideo(vid, data)
	sdv := makeSDVideo(hd, info)
	if sdv == nil || sdv.width != 480 || sdv.seconds != 3 || len(sdv.data) >= len(hd) {
		t.Fatalf("video: %+v", sdv)
	}
	t.Logf("image SD %d KB; video HD %d KB -> SD %d KB (%dx%d)", len(sd.data)/1024, len(hd)/1024, len(sdv.data)/1024, sdv.width, sdv.height)
}
