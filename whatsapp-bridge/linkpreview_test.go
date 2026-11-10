package main

import (
	"os"
	"testing"
)

func TestParseOG(t *testing.T) {
	page := []byte(`<html><head><title>Fallback</title>
<meta property="og:title" content="Título OG">
<meta property="og:description" content="Descrição">
<meta property="og:image" content="/img.jpg"></head><body><meta property="og:title" content="ignored"></body></html>`)
	og := parseOG(page)
	if og["og:title"] != "Título OG" || og["og:description"] != "Descrição" || og["og:image"] != "/img.jpg" || og["<title>"] != "Fallback" {
		t.Fatalf("%v", og)
	}
	if u := firstURL("olha isso: https://exemplo.com/a?b=1)."); u != "https://exemplo.com/a?b=1" {
		t.Fatal(u)
	}
}

// Live fetch of real pages; run with LINKPREVIEW_LIVE=1. Nothing is sent to WhatsApp.
func TestFetchLive(t *testing.T) {
	if os.Getenv("LINKPREVIEW_LIVE") == "" {
		t.Skip("set LINKPREVIEW_LIVE=1")
	}
	for _, u := range []string{"https://www.instagram.com/p/De5NkYwDU-q/", "https://www.mercadolivre.com.br/", "https://github.com/tulir/whatsmeow", "https://g1.globo.com/"} {
		p := fetchLinkPreview(u)
		if p == nil {
			t.Logf("%s: sem prévia", u)
			continue
		}
		small, _, _ := jpegFit(p.image, inlineThumbPx)
		big, w, h := jpegFit(p.image, uploadThumbPx)
		t.Logf("%s\n  título=%q\n  desc=%.80q\n  imagem=%d B, mini=%d B, grande=%dx%d %d B", u, p.title, p.description, len(p.image), len(small), w, h, len(big))
	}
}
