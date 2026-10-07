# Instalador do WhatsApp MCP (Windows).
# Uso (na pasta extraida):  powershell -ExecutionPolicy Bypass -File .\install.ps1
#
# O que faz:
#   1. Instala pre-requisitos via winget (Go, uv, gcc/WinLibs, ffmpeg; Python 3.12 se nao houver >= 3.11)
#   2. Compila o bridge em %USERPROFILE%\.whatsapp-mcp\bin
#   3. Cria o ambiente Python em %USERPROFILE%\.whatsapp-mcp\venv
#   4. Registra o MCP "whatsapp" no Claude Code e/ou Claude Desktop
#   5. Abre a janela do bridge para escanear o QR code
#
# Sessao, mensagens e midias ficam em %USERPROFILE%\.whatsapp-mcp\store (nunca na pasta do codigo).
# Nao usar AppData: o Windows virtualiza AppData para apps empacotados (Claude desktop), e o que o Claude grava
# la fica invisivel para o Explorer e os atalhos de logon.
param(
    [string]$DataDir = (Join-Path $env:USERPROFILE '.whatsapp-mcp'),
    [switch]$SkipPrereqs,
    [switch]$SkipRegister,
    [switch]$SkipPair
)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$DataDir = [IO.Path]::GetFullPath($DataDir)

# Migrate installs from the old location (%LOCALAPPDATA%\whatsapp-mcp). When this runs from Claude desktop, that path
# is the app's private virtualized copy, which is exactly where the old installer put everything. Keeps the WhatsApp
# session (no new QR), messages and auto-markread.json. Bridge must be stopped (done here) so the DBs aren't in use.
$oldDir = Join-Path $env:LOCALAPPDATA 'whatsapp-mcp'
if ((Test-Path (Join-Path $oldDir 'store\whatsapp.db')) -and -not (Test-Path (Join-Path $DataDir 'store\whatsapp.db'))) {
    Write-Host "`n==> Migrando dados de $oldDir para $DataDir" -ForegroundColor Cyan
    Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" | Where-Object { $_.CommandLine -match '-File\s+"?[^"]*start-bridge\.ps1' -and $_.ProcessId -ne $PID } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
    Get-Process whatsapp-bridge -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 2
    New-Item -ItemType Directory -Force $DataDir | Out-Null
    foreach ($item in 'store', 'logs', 'auto-markread.json') {
        $src = Join-Path $oldDir $item
        if (Test-Path $src) { Copy-Item $src (Join-Path $DataDir $item) -Recurse -Force }
    }
    Write-Host "    sessao, mensagens e configuracao copiadas (a pasta antiga foi mantida como backup)"
}

function Step($msg) { Write-Host "`n==> $msg" -ForegroundColor Cyan }
function Refresh-Path {
    $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
    $links = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links'
    if (Test-Path $links) { $env:Path = "$links;$env:Path" }
    $pk = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Packages'
    foreach ($pattern in 'gcc.exe', 'uv.exe', 'ffmpeg.exe') {
        $hit = Get-ChildItem $pk -Recurse -Filter $pattern -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($hit) { $env:Path = "$($hit.DirectoryName);$env:Path" }
    }
    if (Test-Path 'C:\Program Files\Go\bin') { $env:Path = "C:\Program Files\Go\bin;$env:Path" }
}
function Find-Python {
    foreach ($cand in @('python', 'py')) {
        $cmd = Get-Command $cand -ErrorAction SilentlyContinue
        if (-not $cmd -or $cmd.Source -like '*WindowsApps*') { continue }   # ignora o atalho da Microsoft Store
        try { $exe = & $cand -c "import sys; print(sys.executable if sys.version_info >= (3, 11) else '')" } catch { $exe = $null }
        if ($exe) { return "$exe".Trim() }
    }
    return $null
}
function Winget-Install($id) {
    Write-Host "    winget $id"
    winget install -e --id $id --accept-source-agreements --accept-package-agreements --silent | Out-Null
}

# 1. Pre-requisitos -----------------------------------------------------------
Refresh-Path
if (-not $SkipPrereqs) {
    Step 'Instalando pre-requisitos (pode demorar alguns minutos)'
    if (-not (Get-Command go -ErrorAction SilentlyContinue))  { Winget-Install 'GoLang.Go' }
    if (-not (Get-Command uv -ErrorAction SilentlyContinue))  { Winget-Install 'astral-sh.uv' }
    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) { Winget-Install 'BrechtSanders.WinLibs.POSIX.UCRT' }
    if (-not (Get-Command ffmpeg -ErrorAction SilentlyContinue)) { Winget-Install 'Gyan.FFmpeg' }
    if (-not (Find-Python)) { Winget-Install 'Python.Python.3.12' }
    Refresh-Path
}
foreach ($tool in 'go', 'uv', 'gcc') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "Nao encontrei '$tool' no PATH. Feche e abra o PowerShell e rode de novo." }
}
$python = Find-Python
if (-not $python) { throw 'Nao encontrei Python >= 3.11. Feche e abra o PowerShell e rode de novo.' }

# 2. Bridge ------------------------------------------------------------------
Step 'Compilando o bridge'
$bin = Join-Path $DataDir 'bin'
New-Item -ItemType Directory -Force $bin | Out-Null
$exePath = Join-Path $bin 'whatsapp-bridge.exe'
# Stop the background wrapper first (it restarts the bridge 30s after it exits), then the bridge itself.
Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" | Where-Object { $_.CommandLine -like '*start-bridge.ps1*' -and $_.ProcessId -ne $PID } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
Get-Process whatsapp-bridge -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exePath } | Stop-Process -Force
$env:CGO_ENABLED = '1'
Push-Location (Join-Path $root 'whatsapp-bridge')
try {
    go build -o $exePath .
    if ($LASTEXITCODE -ne 0) { throw 'Falha ao compilar o bridge.' }
} finally { Pop-Location }

# 3. Servidor MCP (Python) ----------------------------------------------------
Step 'Criando ambiente Python'
$env:UV_PROJECT_ENVIRONMENT = Join-Path $DataDir 'venv'
Push-Location (Join-Path $root 'whatsapp-mcp-server')
try {
    uv sync --python $python
    if ($LASTEXITCODE -ne 0) { throw 'Falha no uv sync.' }
} finally { Pop-Location }
$venvPy = Join-Path $env:UV_PROJECT_ENVIRONMENT 'Scripts\python.exe'
$mainPy = Join-Path $root 'whatsapp-mcp-server\main.py'
# Calls every read tool through the MCP layer against a throwaway DB (catches output-validation breakage).
& $venvPy (Join-Path $root 'whatsapp-mcp-server\test_tools.py')
if ($LASTEXITCODE -ne 0) { throw 'As ferramentas do MCP falharam no teste (ver acima).' }

# 4. Registro no Claude --------------------------------------------------------
if (-not $SkipRegister) {
    Step 'Registrando no Claude'
    $registered = $false
    if (Get-Command claude -ErrorAction SilentlyContinue) {
        try { claude mcp remove whatsapp -s user | Out-Null } catch { }
        claude mcp add --scope user whatsapp -- $venvPy $mainPy
        Write-Host '    Claude Code: ok (escopo user)'
        $registered = $true
    }
    $desktopDir = Join-Path $env:APPDATA 'Claude'
    if (Test-Path $desktopDir) {
        $cfgPath = Join-Path $desktopDir 'claude_desktop_config.json'
        $cfg = if (Test-Path $cfgPath) { Copy-Item $cfgPath "$cfgPath.bak" -Force; Get-Content $cfgPath -Raw | ConvertFrom-Json } else { New-Object PSObject }
        if (-not $cfg.PSObject.Properties['mcpServers']) { $cfg | Add-Member -NotePropertyName mcpServers -NotePropertyValue (New-Object PSObject) }
        $entry = [pscustomobject]@{ command = $venvPy; args = @($mainPy) }
        $cfg.mcpServers | Add-Member -NotePropertyName whatsapp -NotePropertyValue $entry -Force
        [IO.File]::WriteAllText($cfgPath, ($cfg | ConvertTo-Json -Depth 20))
        Write-Host '    Claude Desktop: ok (reinicie o app para carregar)'
        $registered = $true
    }
    # Rules for every Claude conversation (which tools exist, confirm before sending, don't "fix" the MCP)
    & (Join-Path $root 'register-claude-md.ps1')
    if (-not $registered) {
        Write-Host '    Nao achei Claude Code nem Claude Desktop. Configure manualmente:' -ForegroundColor Yellow
        Write-Host "    comando: $venvPy"
        Write-Host "    args:    $mainPy"
    }
}

# 5. Pareamento ---------------------------------------------------------------
if (-not $SkipPair) {
    Step 'Abrindo o bridge: escaneie o QR code (WhatsApp > Aparelhos conectados > Conectar aparelho)'
    Start-Process powershell -WorkingDirectory $root -ArgumentList '-NoProfile', '-NoExit', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $root 'start-bridge.ps1')
}

Write-Host "`nPronto. Depois de parear, abra uma conversa NOVA no Claude e peca, por exemplo:" -ForegroundColor Green
Write-Host '  "liste minhas conversas mais recentes do WhatsApp"'
