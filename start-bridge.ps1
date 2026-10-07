# Sobe o WhatsApp bridge. Dados (sessao, mensagens, midias) ficam em %LOCALAPPDATA%\whatsapp-mcp, fora da pasta do codigo.
# Uso interativo (pareamento/QR):  powershell -ExecutionPolicy Bypass -File start-bridge.ps1
# Uso em segundo plano:            powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File start-bridge.ps1 -Log
param(
    [switch]$Log,
    [string]$DataDir = (Join-Path $env:LOCALAPPDATA 'whatsapp-mcp')
)

$exe = Join-Path $DataDir 'bin\whatsapp-bridge.exe'
if (-not (Test-Path $exe)) { Write-Error "Bridge nao compilado: $exe (rode install.ps1)"; exit 1 }
$env:WHATSAPP_STORE_DIR = Join-Path $DataDir 'store'

# Uma instancia so: duas sessoes com a mesma chave derrubam uma a outra.
if (Get-Process whatsapp-bridge -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }) { Write-Host 'Bridge ja esta rodando.'; exit 0 }
# Also bail out if another background loop (-Log) is alive: it may just be in its 30s wait before restarting the bridge.
$otherLoop = Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.ProcessId -ne $PID -and $_.CommandLine -like '*start-bridge.ps1*-Log*' }
if ($otherLoop) { Write-Host 'Bridge ja esta rodando (reiniciando em segundo plano).'; exit 0 }

Set-Location $DataDir
if ($Log) {
    $logDir = Join-Path $DataDir 'logs'
    New-Item -ItemType Directory -Force $logDir | Out-Null
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
