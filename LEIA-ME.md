# WhatsApp no Claude — instalação

Liga o seu WhatsApp ao Claude (Claude Code ou Claude Desktop) no seu próprio notebook Windows.
Tudo roda local: suas conversas ficam em `%LOCALAPPDATA%\whatsapp-mcp` e não vão para nenhum servidor além do próprio WhatsApp e do Claude quando você pergunta algo.

## Instalar (uns 10 minutos)

1. Extraia o zip numa pasta (ex.: `Documentos\whatsapp-mcp`).
2. Abra o PowerShell **nessa pasta** e rode:
   ```
   powershell -ExecutionPolicy Bypass -File .\install.ps1
   ```
   Ele instala o que faltar (Go, uv, gcc, ffmpeg, Python), compila e registra no Claude.
3. Vai abrir uma janela com um **QR code**. No celular: WhatsApp → ⋮ → **Aparelhos conectados** → **Conectar um aparelho** → escaneie. (Expira em 3 min; se expirar, feche a janela e rode `.\start-bridge.ps1`.)
4. Espere sincronizar (alguns minutos) e abra uma **conversa nova** no Claude (reinicie o Claude Desktop, se usar ele).

Alternativa: se você usa o Claude Code, pode só abrir esta pasta nele e pedir *"instale este MCP seguindo o LEIA-ME.md"*.

## Usar

Exemplos de pedidos:
- "O que chegou hoje no WhatsApp que precisa de resposta?"
- "Resume o grupo X da última semana"
- "Marca os grupos de promoção como lidos"
- "Manda pro Fulano: ..." (o Claude sempre pede sua confirmação antes de enviar)

## Importante

- **O bridge precisa estar rodando.** Depois de reiniciar o notebook, rode de novo:
  `powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File .\start-bridge.ps1 -Log`
- **Risco de ban:** usa um protocolo não oficial (o mesmo do WhatsApp Web). Uso normal é tranquilo; evite disparar mensagens em massa ou para muitos números novos.
- **Não aprove envio automático.** Qualquer pessoa pode te mandar uma mensagem com "instruções" para o Claude; a confirmação antes de enviar é o que te protege.
- Para desconectar: celular → Aparelhos conectados → remover.

Detalhes técnicos e o que foi corrigido em relação ao projeto original: `SETUP.md`.
