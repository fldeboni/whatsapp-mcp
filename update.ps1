# Atualiza o WhatsApp MCP para a versao mais nova do repositorio e recompila.
# Uso (na pasta do projeto):  powershell -ExecutionPolicy Bypass -File .\update.ps1
# Sessao, mensagens e configuracoes ficam em %USERPROFILE%\.whatsapp-mcp e NAO sao tocadas.
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$repo = 'https://github.com/fldeboni/whatsapp-mcp.git'

if (-not (Test-Path (Join-Path $root '.git'))) {
    Write-Host 'Esta pasta veio de um zip, nao de um clone do repositorio.' -ForegroundColor Yellow
    Write-Host 'Instale uma vez a partir do repositorio (publico, sem login):'
    Write-Host "  git clone $repo"
    Write-Host '  cd whatsapp-mcp'
    Write-Host '  powershell -ExecutionPolicy Bypass -File .\install.ps1 -SkipPair'
    Write-Host 'A sessao do WhatsApp continua valendo (fica em %USERPROFILE%\.whatsapp-mcp), nao precisa escanear o QR de novo.'
    exit 1
}

Push-Location $root
try {
    $before = (git rev-parse HEAD).Trim()
    git pull --ff-only
    if ($LASTEXITCODE -ne 0) {
        # History was rewritten on the server (e.g. author e-mail changed): with no local edits, just follow it.
        if (git status --porcelain) { throw 'git pull falhou: ha alteracoes locais na pasta. Rode "git status" para ver.' }
        Write-Host 'O historico do repositorio foi reescrito no servidor; alinhando esta pasta com ele.' -ForegroundColor Yellow
        git fetch origin
        git reset --hard '@{u}'
        if ($LASTEXITCODE -ne 0) { throw 'Nao consegui alinhar com o repositorio. Rode "git status" para ver.' }
        $before = ''
    }
    $after = (git rev-parse HEAD).Trim()
} finally { Pop-Location }

if ($before -eq $after) { Write-Host 'Ja esta na versao mais nova.' -ForegroundColor Green; exit 0 }

if ($before) {
    Write-Host "`nNovidades:" -ForegroundColor Cyan
    git -C $root log --format='  - %s' "$before..$after"
}

# Recompila e reinstala sem pedir QR (a sessao continua valendo); o install para o bridge antes de compilar.
& (Join-Path $root 'install.ps1') -SkipPrereqs -SkipPair
# Give names to chats stored with just the number (address book syncs after the first messages)
& (Join-Path $env:USERPROFILE '.whatsapp-mcp\venv\Scripts\python.exe') (Join-Path $root 'fix_chat_names.py')
# install.ps1 stopped the bridge and (re)created the shortcuts; it must be restarted by the user, not by Claude.
Write-Host "`nAtualizado. Agora: (1) duplo clique no atalho 'WhatsApp MCP Bridge' da area de trabalho;" -ForegroundColor Green
Write-Host "(2) abra uma conversa NOVA no Claude (as abertas continuam com a versao antiga)." -ForegroundColor Green
