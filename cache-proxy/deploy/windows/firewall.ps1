# Run once as Administrator after the cache server has been verified locally.
$ErrorActionPreference = 'Stop'
$ruleName = 'Apehost-Turbo-Cache-Mesh'
$program = Join-Path $PSScriptRoot 'workerd.exe'
if (-not (Test-Path -LiteralPath $program)) { throw 'Install the cache server first.' }
if (Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue) {
    Remove-NetFirewallRule -Name $ruleName
}
New-NetFirewallRule -Name $ruleName -DisplayName 'Apehost Turbo cache (Cloudflare Mesh)' `
    -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8787 `
    -RemoteAddress '100.96.0.0/12' -Program $program -Profile Any | Out-Null
Set-Content -LiteralPath (Join-Path $PSScriptRoot 'firewall-installed.txt') -Value 'TCP 8787, Cloudflare Mesh 100.96.0.0/12 only'
