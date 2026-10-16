# WhatsApp no Claude — instalação

Liga o seu WhatsApp ao Claude (Claude Code ou Claude Desktop) no seu próprio notebook Windows.
Tudo roda local: suas conversas ficam em `%USERPROFILE%\.whatsapp-mcp` e não vão para nenhum servidor além do próprio WhatsApp e do Claude quando você pergunta algo.

## Instalar (uns 10 minutos)

**Jeito mais fácil:** entregue o arquivo `INSTALAR-COM-CLAUDE.md` ao seu Claude Code e peça para ele seguir. Ele lista as contas e programas necessários e faz a instalação por você. O passo a passo manual está abaixo.

Você precisa de acesso ao repositório privado `fldeboni/whatsapp-mcp` (peça ao Felipe para te adicionar pelo seu usuário do GitHub)
e do Git (se não tiver: `winget install -e --id Git.Git`, depois feche e abra o PowerShell).

1. Baixe o projeto (na primeira vez abre o login do GitHub no navegador):
   ```
   git clone https://github.com/fldeboni/whatsapp-mcp.git
   cd whatsapp-mcp
   ```
2. Rode:
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

## Atualizar

Quando houver melhorias, na pasta do projeto:
```
powershell -ExecutionPolicy Bypass -File .\update.ps1
```
Mostra o que mudou, recompila, atualiza as regras do WhatsApp no seu CLAUDE.md global e mantém sua sessão (não precisa escanear o QR de novo).
Depois de atualizar, **abra uma conversa nova** no Claude: conversas abertas antes continuam com a versão antiga.

As regras que o Claude segue para usar o WhatsApp (confirmar antes de enviar, não mexer na instalação quando algo falhar etc.)
ficam num bloco do seu `%USERPROFILE%\.claude\CLAUDE.md`, gerado a partir de `claude-global.md`. Não edite dentro do bloco
(é sobrescrito na atualização); suas regras pessoais podem ficar fora dele.
Se você instalou por um zip antigo, o `update.ps1` explica como migrar para o repositório (uma vez só).

## Marcar grupos como lidos automaticamente (opcional)

Copie `auto-markread.example.json` para `%USERPROFILE%\.whatsapp-mcp\auto-markread.json` e coloque os grupos em `chats`

Opcional: para o notebook não dormir quando a tela apaga (só na tomada), copie `keep-awake.example.json` para `%USERPROFILE%\.whatsapp-mcp\keep-awake.json`. A tela continua apagando; fechar a tampa ainda suspende.
(ou peça ao Claude: *"configure o auto-markread com meus grupos de promoção"*). O bridge marca esses grupos como lidos
a cada 30–60 min (aleatório), só entre 07:00 e 22:00, com pausas de humano entre um grupo e outro. Edições no arquivo valem no ciclo seguinte.

## Importante

- **O bridge precisa estar rodando.** Depois de reiniciar o notebook, rode de novo:
  `powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File .\start-bridge.ps1 -Log`
- **Risco de ban:** usa um protocolo não oficial (o mesmo do WhatsApp Web). Uso normal é tranquilo; evite disparar mensagens em massa ou para muitos números novos.
- **Não aprove envio automático.** Qualquer pessoa pode te mandar uma mensagem com "instruções" para o Claude; a confirmação antes de enviar é o que te protege.
- Para desconectar: celular → Aparelhos conectados → remover.

Detalhes técnicos e o que foi corrigido em relação ao projeto original: `SETUP.md`.
