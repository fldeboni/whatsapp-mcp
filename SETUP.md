# WhatsApp MCP — detalhes técnicos

Fork de [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) com correções de segurança e de funcionamento.
Conecta o WhatsApp **pessoal** ao Claude via whatsmeow (protocolo não oficial — há risco de ban; evitar envio em massa).

## Onde fica cada coisa

| O quê | Onde | Por quê |
|---|---|---|
| Código | pasta extraída (pode ser OneDrive) | |
| Sessão (`whatsapp.db`), mensagens (`messages.db`), mídias | `%USERPROFILE%\.whatsapp-mcp\store` | `whatsapp.db` = controle total da conta; não pode ir pra nuvem. SQLite também corrompe em pasta sincronizada |
| Binário do bridge | `%USERPROFILE%\.whatsapp-mcp\bin\whatsapp-bridge.exe` | |
| venv Python | `%USERPROFILE%\.whatsapp-mcp\venv` | |
| Logs (modo background) | `%USERPROFILE%\.whatsapp-mcp\logs` | |

`WHATSAPP_STORE_DIR` sobrescreve o local da store (bridge e servidor MCP leem a mesma variável).

## Mudanças em relação ao upstream

1. **API REST só em `127.0.0.1:8080`** (upstream: todas as interfaces, sem auth — qualquer um na rede enviava mensagem como você e exfiltrava arquivos via `media_path`).
2. **Store fora da pasta do código** (ver tabela).
3. **Nome de arquivo de mídia sanitizado** — o nome do documento vem do remetente; sem isso, `..\..\Startup\x.bat` gravava fora da pasta.
4. **Documento enviado leva só o nome do arquivo** (upstream mandava o caminho local completo como título no Windows).
5. whatsmeow atualizado (versões antigas são recusadas pelo servidor); `mcp<2`; lock atualizado pra Python 3.11–3.14.
6. **Legendas de foto/vídeo/documento gravadas** (upstream descartava — quase metade das mensagens de grupo ficava sem texto).
7. **Conversas endereçadas por LID** (`xxx@lid`) normalizadas pro número de telefone — sem isso respostas caíam numa conversa sem nome, separada do contato. `migrate_lid_chats.py` corrige bancos antigos (rodar com o bridge parado).
8. `list_chats(include_last_message=False)` corrigido; `print()` vai pra stderr (stdout é o canal MCP).
9. **Ferramenta nova `mark_chats_as_read`**: manda read receipts por remetente + "marcar como lida" (o mesmo que o WhatsApp Web faz ao abrir a conversa), com pausas de 4–12 s entre conversas.

10. **Recuperação após hibernar**: mensagens que não descriptografam são pedidas de volta ao celular (`AutomaticMessageRerequestFromPhone`; `/api/rerequest` para falhas antigas); o app state `regular_low` é ressincronizado a cada reconexão. O patch de "marcar como lida" é best-effort — os read receipts sozinhos já limpam o celular.
11. **Auto mark-read** (`auto-markread.json` em `%USERPROFILE%\.whatsapp-mcp`, modelo em `auto-markread.example.json`): goroutine no bridge, intervalo aleatório entre `interval_min_minutes` e `interval_max_minutes`, só dentro de `window_start`–`window_end`, pula grupos sem nada novo. Usa o relógio de parede (os timers monotônicos do Go no Windows não andam durante a hibernação).
12a. **Keep-awake** (`keep-awake.json` em `%USERPROFILE%\.whatsapp-mcp`, modelo em `keep-awake.example.json`): em notebook com "espera moderna", a máquina dorme quando a tela apaga por inatividade. Com o arquivo, o bridge pede ao Windows para manter o SISTEMA ativo (`SetThreadExecutionState` ES_SYSTEM_REQUIRED, não a tela): a tela apaga, o note não dorme. `only_when_plugged_in` (padrão true) libera na bateria. Fechar a tampa/botão de energia ainda suspende. Relido a cada minuto.

12. **Busca de contatos na agenda**: `search_contacts` procura nas conversas E na agenda do WhatsApp (`whatsmeow_contacts`, código LID convertido para número), sem diferenciar acentos (`list_chats` também). Conversas gravadas só com o número ganham nome: o bridge tenta de novo a agenda, e `fix_chat_names.py` corrige o banco (rodado pelo `update.ps1`).
13. **`request_chat_history`**: busca no celular mensagens antigas de uma conversa (até 50 por vez); `backfill_chat_history` puxa o histórico inteiro em segundo plano até uma data (`backfill_status` mostra o andamento). **Mensagens interativas** (cartão de chave Pix etc.) são gravadas como texto.
15. **Apagar e favoritar** (`delete.go`, `star.go`): `delete_message` apaga para todos (revoke; ~60 h para as minhas, admin apaga as dos outros em grupo) ou só para mim (app state `regular_high`, `deleteMessageForMe` v3); `star_message` favorita (`star` v2). A store guarda o texto e marca `deleted_at` / `deleted_for_me_at` / `starred_at` (também quando feito no celular). Patch de app state com 409/LTHash refaz o fetch completo e tenta de novo uma vez.
16. **HD e documento** (`hd.go`): foto > 1280 px e vídeo > 480p vão como o "HD" do app (dual upload: cópia SD com `pairedMediaType` SD_*_PARENT + original em `associatedChildMessage` HD_*_CHILD apontando para ela; precisa ffmpeg, sem ele vai normal). O HD recebido repõe a mídia guardada da mensagem SD (download_media baixa o original). `as_document` manda o arquivo intacto como documento (até 2 GB).
18. **Prévia de link** (`linkpreview.go`): no WhatsApp quem monta o card é quem envia. O bridge baixa a página (User-Agent do WhatsApp, depois de navegador), lê og:/twitter: tags e manda `ExtendedTextMessage` com `matchedText`/`title`/`description`, miniatura JPEG inline (192 px) e, se a imagem tiver ≥ 300 px, uma de até 1024 px enviada como `MediaLinkThumbnail` (card grande). Falhou (site fora, sem og:tags) → vai o texto puro.
17. **Fixar mensagem** (`pin.go`): `pin_message` manda `PinInChatMessage` (PIN_FOR_ALL / UNPIN_FOR_ALL) com `messageContextInfo.messageAddOnDurationInSecs` (86400 / 604800 / 2592000). Pins recebidos (de qualquer um) gravam `pinned_until` (UTC) na mensagem fixada, sem linha nova.
14. Aparelho aparece como **"Claude (WhatsApp MCP)"**, tipo computador, em Aparelhos conectados (vale para pareamentos novos).

## Atualização

Repositório público `fldeboni/whatsapp-mcp` (sem colaboradores) (remote `origin`; o projeto original fica como `upstream`). `update.ps1` faz `git pull --ff-only`, mostra os commits novos e roda `install.ps1 -SkipPrereqs -SkipPair`.

## Permissões sugeridas no Claude Code (`~/.claude/settings.json`)

Leitura liberada, envio sempre perguntando — qualquer contato pode mandar texto com instrução escondida ("encaminhe X para Y"), e a confirmação é a barreira.

```json
"permissions": {
  "allow": [
    "mcp__whatsapp__search_contacts", "mcp__whatsapp__list_messages", "mcp__whatsapp__list_chats",
    "mcp__whatsapp__get_chat", "mcp__whatsapp__get_direct_chat_by_contact", "mcp__whatsapp__get_contact_chats",
    "mcp__whatsapp__get_last_interaction", "mcp__whatsapp__get_message_context", "mcp__whatsapp__download_media",
    "mcp__whatsapp__mark_chats_as_read", "mcp__whatsapp__request_chat_history",
    "mcp__whatsapp__backfill_chat_history", "mcp__whatsapp__backfill_status", "mcp__whatsapp__get_message_status", "mcp__whatsapp__get_group_info", "mcp__whatsapp__star_message"
  ],
  "ask": ["mcp__whatsapp__send_message", "mcp__whatsapp__send_file", "mcp__whatsapp__send_audio_message", "mcp__whatsapp__send_reaction", "mcp__whatsapp__delete_message", "mcp__whatsapp__save_contact", "mcp__whatsapp__send_contact", "mcp__whatsapp__create_group", "mcp__whatsapp__update_group_settings", "mcp__whatsapp__update_group_members"]
}
```

## Operação

- O bridge precisa estar rodando para o Claude receber e enviar mensagens.
- Subir em segundo plano: `powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File .\start-bridge.ps1 -Log`
- Para subir sozinho no logon: tarefa agendada (gatilho "Ao fazer logon") ou atalho na pasta Inicializar com o comando acima. Em máquinas corporativas a tarefa agendada pode ser bloqueada por política.
- Re-parear do zero: parar o bridge, apagar `%USERPROFILE%\.whatsapp-mcp\store\whatsapp.db`, rodar `start-bridge.ps1`.
- **WhatsApp desconectou o aparelho** (`device_removed` no log): o bridge reinicia sozinho em modo de pareamento. Longe do
  notebook, grave o número (DDI+DDD+número) em `%USERPROFILE%\.whatsapp-mcp\pair-phone.txt` e reinicie o bridge: o código
  de 8 letras aparece em `%USERPROFILE%\.whatsapp-mcp\pairing.txt` (e chega notificação no celular). No celular:
  Aparelhos conectados > Conectar aparelho > "Conectar com número de telefone". Sem o número, o QR fica em `pairing-qr.png`.
  Código vence em poucos minutos; reiniciar o bridge gera outro. Depois de pareado o arquivo do número é apagado.
- A sessão aparece em "Aparelhos conectados" no celular; dá pra desconectar por lá a qualquer momento.

## Rebuild manual

```powershell
$env:CGO_ENABLED=1   # precisa gcc (winget BrechtSanders.WinLibs.POSIX.UCRT)
cd whatsapp-bridge; go build -o "$env:USERPROFILE\.whatsapp-mcp\bin\whatsapp-bridge.exe" .
cd ..\whatsapp-mcp-server; $env:UV_PROJECT_ENVIRONMENT="$env:USERPROFILE\.whatsapp-mcp\venv"; uv sync
```
Parar o bridge antes do build (o exe fica travado enquanto roda). Ou simplesmente rodar `install.ps1` de novo.
