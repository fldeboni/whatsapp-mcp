# Grava/atualiza o bloco do WhatsApp MCP no CLAUDE.md global (~/.claude/CLAUDE.md), para que toda
# conversa do Claude saiba quais ferramentas existem e o que fazer (e nao fazer) com elas.
# O bloco fica entre marcadores e e substituido a cada instalacao/atualizacao; o resto do arquivo nao e tocado.
# Chamado pelo install.ps1. Uso manual: powershell -ExecutionPolicy Bypass -File .\register-claude-md.ps1
param([string]$ClaudeMd = (Join-Path $env:USERPROFILE '.claude\CLAUDE.md'))
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$start = '<!-- whatsapp-mcp:start (gerado pelo install.ps1 do whatsapp-mcp; nao edite aqui, e sobrescrito na atualizacao) -->'
$end = '<!-- whatsapp-mcp:end -->'

$utf8 = New-Object System.Text.UTF8Encoding($false)
$template = [IO.File]::ReadAllText((Join-Path $root 'claude-global.md'), $utf8).Replace('{{PROJECT_DIR}}', $root).TrimEnd()
$block = "$start`n$template`n$end"

New-Item -ItemType Directory -Force (Split-Path $ClaudeMd) | Out-Null
$current = if (Test-Path $ClaudeMd) { [IO.File]::ReadAllText($ClaudeMd, $utf8) } else { '' }

$pattern = '(?s)<!-- whatsapp-mcp:start.*?<!-- whatsapp-mcp:end -->'
if ($current -match $pattern) {
    $updated = [regex]::Replace($current, $pattern, { param($m) $block })
    $action = 'atualizado'
} else {
    $sep = if ($current.Trim()) { "`n`n" } else { '' }
    $updated = $current.TrimEnd() + $sep + $block + "`n"
    $action = 'adicionado'
}
if ($updated -ne $current) {
    if (Test-Path $ClaudeMd) { Copy-Item $ClaudeMd "$ClaudeMd.bak" -Force }
    [IO.File]::WriteAllText($ClaudeMd, $updated, $utf8)
    Write-Host "    CLAUDE.md global: bloco do WhatsApp MCP $action ($ClaudeMd)"
} else {
    Write-Host '    CLAUDE.md global: ja estava em dia'
}
