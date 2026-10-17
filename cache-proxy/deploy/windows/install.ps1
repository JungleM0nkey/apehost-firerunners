param(
    [string]$MeshAddress = '100.96.0.4',
    [string]$InstallRoot = (Join-Path $env:LOCALAPPDATA 'apehost\turbo-cache')
)
$ErrorActionPreference = 'Stop'
$powershellPath = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (-not (Test-Path -LiteralPath $powershellPath)) { throw 'Windows PowerShell is required.' }
$packageRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$workerBinary = Join-Path $packageRoot 'node_modules\@cloudflare\workerd-windows-64\bin\workerd.exe'
if (-not (Test-Path -LiteralPath $workerBinary)) { throw 'Run npm ci in cache-proxy first.' }
if (-not (Test-Path -LiteralPath (Join-Path $packageRoot 'dist\worker.js'))) { throw 'Run npm run build first.' }
$meshIp = [Net.IPAddress]::Parse($MeshAddress)
if ($meshIp.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork -or $MeshAddress -notmatch '^100\.(9[6-9]|10[0-9]|11[01])\.') {
    throw 'MeshAddress must be in 100.96.0.0/12.'
}
New-Item -ItemType Directory -Path $InstallRoot -Force | Out-Null
$InstallRoot = (Resolve-Path -LiteralPath $InstallRoot).Path
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$userRule = '*' + $identity.User.Value + ':(OI)(CI)F'
& icacls.exe $InstallRoot /inheritance:r /grant:r $userRule '*S-1-5-18:(OI)(CI)F' /Q | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not protect the cache configuration directory.' }
$pidFile = Join-Path $InstallRoot 'worker.pid'
if (Test-Path -LiteralPath $pidFile) {
    $existing = Get-Process -Id ([int](Get-Content -LiteralPath $pidFile)) -ErrorAction SilentlyContinue
    if ($existing -and $existing.Path -eq (Join-Path $InstallRoot 'workerd.exe')) {
        Stop-Process -Id $existing.Id
        $existing.WaitForExit()
    }
}
foreach ($directory in @('artifacts', 'index')) {
    New-Item -ItemType Directory -Path (Join-Path $InstallRoot $directory) -Force | Out-Null
}
Copy-Item -LiteralPath $workerBinary -Destination (Join-Path $InstallRoot 'workerd.exe') -Force
Copy-Item -LiteralPath (Join-Path $packageRoot 'dist\worker.js') -Destination $InstallRoot -Force
foreach ($name in @('config.capnp', 'launch.ps1', 'firewall.ps1')) {
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $InstallRoot -Force
}
function New-CacheSecret {
    $bytes = New-Object byte[] 32
    $random = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $random.GetBytes($bytes) } finally { $random.Dispose() }
    [Convert]::ToBase64String($bytes)
}
$serverEnv = Join-Path $InstallRoot 'server.env'
if (-not (Test-Path -LiteralPath $serverEnv)) {
    $writeToken = New-CacheSecret
    $readToken = New-CacheSecret
    $signatureKey = New-CacheSecret
    $common = "TURBO_API=http://${MeshAddress}:8787`nTURBO_TEAM=apehost-shell`nTURBO_REMOTE_CACHE_SIGNATURE_KEY=$signatureKey`n"
    [IO.File]::WriteAllText((Join-Path $InstallRoot 'client.env'), $common + "TURBO_TOKEN=$writeToken`nTURBO_CACHE=remote:rw`n")
    [IO.File]::WriteAllText((Join-Path $InstallRoot 'client-readonly.env'), $common + "TURBO_TOKEN=$readToken`nTURBO_CACHE=remote:r`n")
    [IO.File]::WriteAllText($serverEnv, "TOKENS_RW=$writeToken`nTOKENS_RO=$readToken`n")
}
$shell = New-Object -ComObject WScript.Shell
$shortcutPath = Join-Path ([Environment]::GetFolderPath('Startup')) 'Apehost Turbo Cache.lnk'
$shortcut = $shell.CreateShortcut($shortcutPath)
$shortcut.TargetPath = $powershellPath
$shortcut.Arguments = '-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "' + (Join-Path $InstallRoot 'launch.ps1') + '"'
$shortcut.WorkingDirectory = $InstallRoot
$shortcut.Description = 'Shared Turbo cache over Cloudflare Mesh'
$shortcut.Save()
$launcher = Start-Process -FilePath $powershellPath -WindowStyle Hidden `
    -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"' + (Join-Path $InstallRoot 'launch.ps1') + '"')) -PassThru
Write-Output "Cache installed at $InstallRoot; launcher PID $($launcher.Id)."
Write-Output "Mesh endpoint: http://${MeshAddress}:8787. Client settings: client.env and client-readonly.env."
