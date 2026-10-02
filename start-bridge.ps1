# Sobe o WhatsApp bridge. Dados (sessão, mensagens, mídias) ficam em %LOCALAPPDATA%\whatsapp-mcp, fora do OneDrive.
# Uso interativo (pareamento/QR):  powershell -File start-bridge.ps1
# Uso em segundo plano (logon):    powershell -WindowStyle Hidden -File start-bridge.ps1 -Log
param([switch]$Log)

$base = Join-Path $env:LOCALAPPDATA 'whatsapp-mcp'
$exe  = Join-Path $base 'bin\whatsapp-bridge.exe'
if (-not (Test-Path $exe)) { Write-Error "Bridge não compilado: $exe (ver SETUP.md)"; exit 1 }

# Uma instância só: duas sessões com a mesma chave derrubam uma à outra.
if (Get-Process whatsapp-bridge -ErrorAction SilentlyContinue) { Write-Host 'Bridge já está rodando.'; exit 0 }

Set-Location $base
if ($Log) {
    $logDir = Join-Path $base 'logs'
    New-Item -ItemType Directory -Force $logDir | Out-Null
    $logFile = Join-Path $logDir ("bridge-{0:yyyyMMdd}.log" -f (Get-Date))
    # Reconecta sozinho se cair (rede, sleep do notebook).
    while ($true) {
        & $exe *>> $logFile
        Add-Content $logFile "[$(Get-Date -Format s)] bridge saiu (exit $LASTEXITCODE), reiniciando em 30s"
        Start-Sleep -Seconds 30
    }
} else {
    & $exe
}
