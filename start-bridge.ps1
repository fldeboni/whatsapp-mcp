# Sobe o WhatsApp bridge. Dados (sessao, mensagens, midias) ficam em %USERPROFILE%\.whatsapp-mcp, fora da pasta do codigo.
# Uso interativo (pareamento/QR):  powershell -ExecutionPolicy Bypass -File start-bridge.ps1
# Uso em segundo plano:            powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File start-bridge.ps1 -Log
param(
    [switch]$Log,
    [string]$DataDir = (Join-Path $env:USERPROFILE '.whatsapp-mcp')
)

# Every launch (shortcut, logon, manual) leaves a trace here, including errors, so a silent exit can be diagnosed.
$launchLog = Join-Path $DataDir 'logs\start-bridge-launches.log'
New-Item -ItemType Directory -Force (Split-Path $launchLog) | Out-Null
function Trace($msg) { "[$(Get-Date -Format s)] PID $PID pai $((Get-CimInstance Win32_Process -Filter "ProcessId=$PID" -ErrorAction SilentlyContinue).ParentProcessId) | $msg" | Out-File $launchLog -Append -Encoding utf8 }
trap { Trace "ERRO: $_"; break }
Trace "inicio (Log=$Log, cwd=$(Get-Location), user=$env:USERNAME)"

$exe = Join-Path $DataDir 'bin\whatsapp-bridge.exe'
if (-not (Test-Path $exe)) { Trace "exe nao encontrado: $exe"; Write-Error "Bridge nao compilado: $exe (rode install.ps1)"; exit 1 }
$env:WHATSAPP_STORE_DIR = Join-Path $DataDir 'store'

# Uma instancia so: duas sessoes com a mesma chave derrubam uma a outra.
if (Get-Process whatsapp-bridge -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }) { Trace 'bridge ja rodando, saindo'; Write-Host 'Bridge ja esta rodando.'; exit 0 }
Set-Location $DataDir
if ($Log) {
    $logDir = Join-Path $DataDir 'logs'
    New-Item -ItemType Directory -Force $logDir | Out-Null
    # One background loop per user: a named mutex (released by Windows when the process dies) instead of
    # scanning command lines, which matched unrelated shells and let two simultaneous starts cancel each other.
    $mutex = New-Object System.Threading.Mutex($false, 'Local\WhatsAppMcpBridgeLoop')
    $owned = $false
    try { $owned = $mutex.WaitOne(0) } catch [System.Threading.AbandonedMutexException] { $owned = $true }
    if (-not $owned) {
        Trace "outro loop ativo, saindo"
        "[$(Get-Date -Format s)] start-bridge: outro loop ja esta ativo (talvez na espera de 30s), nada a fazer" |
            Out-File -FilePath (Join-Path $logDir ("bridge-{0:yyyyMMdd}.log" -f (Get-Date))) -Append -Encoding utf8
        Write-Host 'Bridge ja esta rodando (reiniciando em segundo plano).'
        exit 0
    }
    Trace "loop iniciado"
    "[$(Get-Date -Format s)] start-bridge: loop iniciado (PID $PID)" |
        Out-File -FilePath (Join-Path $logDir ("bridge-{0:yyyyMMdd}.log" -f (Get-Date))) -Append -Encoding utf8
    # Reconecta sozinho se cair (rede, sleep do notebook).
    while ($true) {
        $logFile = Join-Path $logDir ("bridge-{0:yyyyMMdd}.log" -f (Get-Date))
        & $exe 2>&1 | ForEach-Object { "$_" } | Out-File -FilePath $logFile -Append -Encoding utf8
        "[$(Get-Date -Format s)] bridge saiu (exit $LASTEXITCODE), reiniciando em 30s" | Out-File -FilePath $logFile -Append -Encoding utf8
        Start-Sleep -Seconds 30
    }
} else {
    & $exe
}
