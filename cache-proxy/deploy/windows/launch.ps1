$ErrorActionPreference = 'Stop'
$cacheRoot = $PSScriptRoot
$pidFile = Join-Path $cacheRoot 'worker.pid'
if (Test-Path -LiteralPath $pidFile) {
    $existing = Get-Process -Id ([int](Get-Content -LiteralPath $pidFile)) -ErrorAction SilentlyContinue
    if ($existing -and $existing.Path -eq (Join-Path $cacheRoot 'workerd.exe')) { exit 0 }
}
foreach ($line in Get-Content -LiteralPath (Join-Path $cacheRoot 'server.env')) {
    $parts = $line.Split('=', 2)
    if ($parts.Count -eq 2 -and $parts[0] -in @('TOKENS_RW', 'TOKENS_RO')) {
        [Environment]::SetEnvironmentVariable($parts[0], $parts[1], 'Process')
    }
}
$worker = Start-Process -FilePath (Join-Path $cacheRoot 'workerd.exe') `
    -ArgumentList @('serve', 'config.capnp') -WorkingDirectory $cacheRoot -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $cacheRoot 'stdout.log') `
    -RedirectStandardError (Join-Path $cacheRoot 'stderr.log') -PassThru
Set-Content -LiteralPath $pidFile -Value $worker.Id
$worker.WaitForExit()
exit $worker.ExitCode
