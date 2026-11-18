param(
    [switch]$Background,
    [int]$DatabasePort = 33316
)
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$sha = [Security.Cryptography.SHA256]::Create()
try { $workspaceKey = [BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($repo))).Replace('-', '').Substring(0, 12) }
finally { $sha.Dispose() }
$runtime = Join-Path $env:LOCALAPPDATA "qushuiyin-m1-$workspaceKey"
$mysqlData = Join-Path $runtime 'mysql'
$state = Join-Path $runtime 'local.json'
$log = Join-Path $runtime 'mysql.log'
$init = Join-Path $runtime 'init.sql'
$go = Get-Command go.exe -ErrorAction SilentlyContinue
$goExe = if ($go) { $go.Source } else { 'D:\Go\bin\go.exe' }
$mysql = (Get-Command mysqld.exe -ErrorAction Stop).Source

function Test-Port([int]$port) {
    $client = [Net.Sockets.TcpClient]::new()
    try { return $client.ConnectAsync('127.0.0.1', $port).Wait(200) -and $client.Connected }
    catch { return $false }
    finally { $client.Dispose() }
}
function New-Key {
    $bytes = New-Object byte[] 32
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes); return [Convert]::ToBase64String($bytes) }
    finally { $rng.Dispose() }
}
function Invoke-ServerCommand([string[]]$CommandArgs) {
    & $script:exe @CommandArgs
    if ($LASTEXITCODE -ne 0) { throw 'Mock server command failed' }
}

New-Item -ItemType Directory -Path $runtime -Force | Out-Null
if (Test-Port 18080) { throw 'Port 18080 is occupied. Stop the existing mock API before starting another.' }
if (Test-Path -LiteralPath $state) {
    $settings = Get-Content -Raw -LiteralPath $state | ConvertFrom-Json
    if ($settings.DatabasePort -ne $DatabasePort) { throw 'The existing mock database uses another port.' }
} else {
    if ((Test-Path -LiteralPath $mysqlData) -or (Test-Port $DatabasePort)) { throw 'Mock database directory or port is occupied.' }
    $settings = [ordered]@{ DatabasePort = $DatabasePort; Password = [Guid]::NewGuid().ToString('N') + [Guid]::NewGuid().ToString('N'); SessionKey = New-Key; TicketKey = New-Key }
    $dbInit = Start-Process -FilePath $mysql -ArgumentList @('--no-defaults','--initialize',"--datadir=`"$mysqlData`"", "--log-error=`"$log`"") -WindowStyle Hidden -PassThru -Wait
    if ($dbInit.ExitCode -ne 0) { throw 'Cannot initialize isolated MySQL.' }
    $settings | ConvertTo-Json | Set-Content -LiteralPath $state -Encoding UTF8
    @"
CREATE DATABASE qushuiyin_dev CHARACTER SET utf8mb4;
CREATE DATABASE qushuiyin_test CHARACTER SET utf8mb4;
CREATE USER 'qsy_mock'@'127.0.0.1' IDENTIFIED BY '$($settings.Password)';
GRANT ALL ON qushuiyin_dev.* TO 'qsy_mock'@'127.0.0.1';
GRANT ALL ON qushuiyin_test.* TO 'qsy_mock'@'127.0.0.1';
"@ | Set-Content -LiteralPath $init -Encoding ASCII
}
if (-not (Test-Port $DatabasePort)) {
    $argsDb = @('--no-defaults',"--datadir=`"$mysqlData`"", "--log-error=`"$log`"", '--bind-address=127.0.0.1', "--port=$DatabasePort", '--mysqlx=OFF', '--skip-name-resolve')
    if (Test-Path -LiteralPath $init) { $argsDb += "--init-file=`"$init`"" }
    Start-Process -FilePath $mysql -ArgumentList $argsDb -WindowStyle Hidden -RedirectStandardError (Join-Path $runtime 'mysql-stderr.log')
    for ($i = 0; $i -lt 60 -and -not (Test-Port $DatabasePort); $i++) { Start-Sleep -Milliseconds 300 }
    if (-not (Test-Port $DatabasePort)) { throw 'Isolated MySQL did not start.' }
} else {
    $ownsPort = @(Get-CimInstance Win32_Process -Filter "name='mysqld.exe'" | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($mysqlData) -and $_.CommandLine -match "--port=$DatabasePort(?:\s|$)" }).Count -gt 0
    if (-not $ownsPort) { throw 'Database port is owned by another instance.' }
}

$env:APP_ENV = 'local'
$env:PARSE_MODE = 'mock'
$env:WECHAT_DEV_LOGIN = 'true'
$env:HTTP_ADDR = '127.0.0.1:18080'
$env:MOCK_MEDIA_ORIGIN = 'http://127.0.0.1:18080'
$env:MYSQL_DSN = "qsy_mock:$($settings.Password)@tcp(127.0.0.1:$DatabasePort)/qushuiyin_dev?timeout=5s&readTimeout=5s&writeTimeout=5s"
$env:HMAC_KEY = $settings.SessionKey
$env:TICKET_SIGNING_KEY = $settings.TicketKey
$script:exe = Join-Path $repo 'bin/qushuiyin-server.exe'
Push-Location $repo
try {
    & $goExe build -o $script:exe ./cmd/qushuiyin-server
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed' }
    Invoke-ServerCommand @('migrate','up')
    if (Test-Path -LiteralPath $init) { Remove-Item -LiteralPath $init }
    Invoke-ServerCommand @('dev','seed')
    if ($Background) {
        $server = Start-Process -FilePath $script:exe -ArgumentList 'serve' -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runtime 'server.log') -RedirectStandardError (Join-Path $runtime 'server-error.log')
        for ($i = 0; $i -lt 40 -and -not (Test-Port 18080); $i++) { if ($server.HasExited) { throw 'Mock server exited; see the local error log.' }; Start-Sleep -Milliseconds 200 }
        if (-not (Test-Port 18080)) { throw 'Mock server did not start.' }
        Write-Output 'Mock API: http://127.0.0.1:18080/v1/config'
    } else {
        Invoke-ServerCommand @('serve')
    }
} finally {
    Pop-Location
}
