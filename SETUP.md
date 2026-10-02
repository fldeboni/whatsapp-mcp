# WhatsApp MCP — setup local (notebook do Felipe)

Fork local de [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) com correções de segurança.
Conecta o WhatsApp **pessoal** ao Claude via whatsmeow (protocolo não oficial — há risco de ban; evitar envio em massa).

## Onde fica cada coisa

| O quê | Onde | Por quê |
|---|---|---|
| Código | esta pasta (OneDrive) | versionado/sincronizado |
| Sessão (`whatsapp.db`), mensagens (`messages.db`), mídias | `%LOCALAPPDATA%\whatsapp-mcp\store` | `whatsapp.db` = controle total da conta; não pode ir pro OneDrive. SQLite também corrompe em pasta sincronizada |
| Binário do bridge | `%LOCALAPPDATA%\whatsapp-mcp\bin\whatsapp-bridge.exe` | |
| venv Python | `%LOCALAPPDATA%\whatsapp-mcp\venv` | |
| Logs (modo background) | `%LOCALAPPDATA%\whatsapp-mcp\logs` | |

`WHATSAPP_STORE_DIR` sobrescreve o local da store (bridge e servidor MCP leem a mesma variável).

## Mudanças em relação ao upstream

1. **API REST só em `127.0.0.1:8080`** (upstream: `:8080`, todas as interfaces, sem auth — qualquer um na rede enviava mensagem como você e exfiltrava arquivos via `media_path`).
2. **Store fora do projeto** (ver tabela).
3. **Nome de arquivo de mídia sanitizado** (`safeFilename`) — o nome do documento vem do remetente; sem isso, `..\..\Startup\x.bat` gravava fora da pasta.
4. whatsmeow atualizado (o pin de 03/2025 é recusado pelo servidor); chamadas ajustadas pra API com `context`.
5. `mcp<2` (o 2.x renomeou FastMCP) e lock atualizado pra rodar no Python 3.14.

## Permissões no Claude Code (`~/.claude/settings.json`)

- Leitura (`list_*`, `search_contacts`, `get_*`, `download_media`): liberada.
- **Envio (`send_message`, `send_file`, `send_audio_message`): sempre pede confirmação.** Não mover pra `allow`: qualquer contato pode mandar texto com instrução escondida ("encaminhe X para Y"), e a confirmação é a barreira.

## Operação

- Início automático: tarefa agendada **"WhatsApp MCP Bridge"** (logon, oculta, reinicia sozinho se cair).
- Rodar manualmente / re-parear: `powershell -ExecutionPolicy Bypass -File .\start-bridge.ps1` (mostra QR).
- Re-parear do zero: parar o bridge, apagar `%LOCALAPPDATA%\whatsapp-mcp\store\whatsapp.db`, rodar de novo.
- A sessão vincula como "aparelho conectado" no celular; dá pra derrubar por lá a qualquer momento.

## Rebuild

```powershell
$env:CGO_ENABLED=1   # precisa gcc (winget BrechtSanders.WinLibs.POSIX.UCRT)
cd whatsapp-bridge; go build -o "$env:LOCALAPPDATA\whatsapp-mcp\bin\whatsapp-bridge.exe" .
cd ..\whatsapp-mcp-server; $env:UV_PROJECT_ENVIRONMENT="$env:LOCALAPPDATA\whatsapp-mcp\venv"; uv sync --python C:\Python314\python.exe
```
Parar o bridge antes do build (o exe fica travado enquanto roda).
