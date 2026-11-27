$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

$account = 'sshore-fixture'
$port = 22222
$openssh = Join-Path $env:SystemRoot 'System32\OpenSSH'
$state = Join-Path $env:ProgramData 'ssh'
$key = Join-Path $env:RUNNER_TEMP 'sshore-fixture-key'
$knownHosts = Join-Path $env:RUNNER_TEMP 'sshore-fixture-hosts'

if (-not (Get-Service sshd -ErrorAction SilentlyContinue)) {
    Add-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0' | Out-Null
}
Stop-Service sshd
New-Item -ItemType Directory -Force $state | Out-Null
New-LocalUser -Name $account -Password (
    ConvertTo-SecureString ([guid]::NewGuid().ToString() + 'aA9!') -AsPlainText -Force
) | Out-Null

& "$openssh\ssh-keygen.exe" -q -A
& "$openssh\ssh-keygen.exe" -q -t ed25519 -N '' -f $key
$authorizedKeys = Join-Path $state 'sshore-fixture-authorized-keys'
Copy-Item "$key.pub" $authorizedKeys
$fixtureUser = "$env:COMPUTERNAME\${account}"
icacls.exe $authorizedKeys /inheritance:r /grant:r "${fixtureUser}:R" '*S-1-5-18:F' '*S-1-5-32-544:F'
icacls.exe $authorizedKeys /setowner $fixtureUser

$keysPath = $authorizedKeys.Replace('\', '/')
@"
ListenAddress 127.0.0.1
Port $port
AllowUsers $account
AuthorizedKeysFile "$keysPath"
PasswordAuthentication no
AllowTcpForwarding local
Subsystem sftp sftp-server.exe
"@ | Set-Content -Encoding ascii (Join-Path $state 'sshd_config')
& "$openssh\sshd.exe" -t
Start-Service sshd

$hostKey = (Get-Content (Join-Path $state 'ssh_host_ed25519_key.pub')).Split(' ')
"[127.0.0.1]:$port $($hostKey[0]) $($hostKey[1])" | Set-Content -Encoding ascii $knownHosts
"SSHORE_TEST_WINDOWS_SSH_KEY=$key" >> $env:GITHUB_ENV
"SSHORE_TEST_WINDOWS_SSH_HOSTS=$knownHosts" >> $env:GITHUB_ENV
