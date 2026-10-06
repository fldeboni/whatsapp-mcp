# Atualiza o WhatsApp MCP para a versao mais nova do repositorio e recompila.
# Uso (na pasta do projeto):  powershell -ExecutionPolicy Bypass -File .\update.ps1
# Sessao, mensagens e configuracoes ficam em %LOCALAPPDATA%\whatsapp-mcp e NAO sao tocadas.
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$repo = 'https://github.com/fldeboni/whatsapp-mcp.git'

if (-not (Test-Path (Join-Path $root '.git'))) {
    Write-Host 'Esta pasta veio de um zip, nao de um clone do repositorio.' -ForegroundColor Yellow
    Write-Host 'Instale uma vez a partir do repositorio (pede login no GitHub na primeira vez):'
    Write-Host "  git clone $repo"
    Write-Host '  cd whatsapp-mcp'
    Write-Host '  powershell -ExecutionPolicy Bypass -File .\install.ps1 -SkipPair'
    Write-Host 'A sessao do WhatsApp continua valendo (fica em %LOCALAPPDATA%), nao precisa escanear o QR de novo.'
    exit 1
}

Push-Location $root
try {
    $before = (git rev-parse HEAD).Trim()
    git pull --ff-only
    if ($LASTEXITCODE -ne 0) { throw 'git pull falhou (alteracoes locais na pasta?). Rode "git status" para ver.' }
    $after = (git rev-parse HEAD).Trim()
} finally { Pop-Location }

if ($before -eq $after) { Write-Host 'Ja esta na versao mais nova.' -ForegroundColor Green; exit 0 }

Write-Host "`nNovidades:" -ForegroundColor Cyan
git -C $root log --format='  - %s' "$before..$after"

# Recompila e reinstala sem pedir QR (a sessao continua valendo); o install para o bridge antes de compilar.
& (Join-Path $root 'install.ps1') -SkipPrereqs -SkipPair
# Give names to chats stored with just the number (address book syncs after the first messages)
& (Join-Path $env:LOCALAPPDATA 'whatsapp-mcpenv\Scripts\python.exe') (Join-Path $root 'fix_chat_names.py')
Write-Host "`nSuba o bridge de novo:" -ForegroundColor Green
Write-Host '  powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File .\start-bridge.ps1 -Log'
