package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"rsc.io/qr"
)

// Pairing without looking at the bridge's terminal: the QR is also saved as a PNG, and when
// %USERPROFILE%\.whatsapp-mcp\pair-phone.txt holds the account's number (DDI+DDD+number) the bridge asks WhatsApp
// for an 8-character pairing code instead. The phone gets a notification; the code goes in
// Aparelhos conectados > Conectar aparelho > "Conectar com número de telefone". Useful when the user is away
// from the notebook and only has the phone.

func dataDir() string { return filepath.Dir(storeDir()) }

func pairPhonePath() string { return filepath.Join(dataDir(), "pair-phone.txt") }

func pairStatusPath() string { return filepath.Join(dataDir(), "pairing.txt") }

func pairQRPath() string { return filepath.Join(dataDir(), "pairing-qr.png") }

func pairPhone() string {
	if p := os.Getenv("WA_PAIR_PHONE"); p != "" {
		return cleanPhone(p)
	}
	b, err := os.ReadFile(pairPhonePath())
	if err != nil {
		return ""
	}
	return cleanPhone(strings.TrimSpace(string(b)))
}

func writePairStatus(lines ...string) {
	text := fmt.Sprintf("[%s]\n%s\n", time.Now().Format("2006-01-02 15:04:05"), strings.Join(lines, "\n"))
	os.WriteFile(pairStatusPath(), []byte(text), 0600)
}

// onPairQR runs for every QR the server hands out: saves it as PNG and, once per bridge run, requests the
// phone-number pairing code (a new run after the ~3 min QR window gives a fresh code).
func onPairQR(client *whatsmeow.Client, code string, codeRequested *bool) {
	if c, err := qr.Encode(code, qr.M); err == nil {
		c.Scale = 8
		os.WriteFile(pairQRPath(), c.PNG(), 0600)
	}
	phone := pairPhone()
	if phone == "" {
		writePairStatus("Aguardando pareamento por QR: abra "+pairQRPath()+" e escaneie em Aparelhos conectados.",
			"Para usar código no lugar do QR, grave o número (ex.: 5527999999999) em "+pairPhonePath())
		return
	}
	if *codeRequested {
		return
	}
	*codeRequested = true
	pc, err := client.PairPhone(context.Background(), phone, true, whatsmeow.PairClientChrome, "Chrome (Windows)")
	if err != nil {
		writePairStatus("Falha ao pedir código de pareamento para "+phone+": "+err.Error(),
			"QR continua disponível em "+pairQRPath())
		fmt.Println("Pairing code request failed:", err)
		return
	}
	writePairStatus("CÓDIGO DE PAREAMENTO: "+pc,
		"Número: "+phone,
		"No celular: Aparelhos conectados > Conectar aparelho > Conectar com número de telefone, e digite o código.",
		"Vale por poucos minutos; se vencer, um novo código aparece aqui.")
	fmt.Println("\nPairing code:", pc)
}

// onPaired cleans the pairing files so the number is not reused by accident.
func onPaired() {
	os.Remove(pairPhonePath())
	os.Remove(pairQRPath())
	writePairStatus("Pareado com sucesso.")
}
