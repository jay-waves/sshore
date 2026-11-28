#requires -Version 5.1
<#
.SYNOPSIS
Install the Windows sshore client without requiring elevation.
.DESCRIPTION
Windows amd64 only; supports PowerShell 5.1 and 7. Updates the first sshore.exe
on PATH; otherwise install to %LOCALAPPDATA%\sshore\bin. Add a new or explicit
install directory to the user and current session PATH. Verify SHA256SUMS and executable version before
replacement. Never modify system PATH, profiles or execution
policy; never elevate or create versioned backups. The installer stops the account daemon
before replacement after reviewing its tasks.
If replacement fails because the executable is in use, close its remaining clients and retry.
.PARAMETER Version
Published X.Y.Z version (without v). Defaults to the latest release.
.EXAMPLE
.\install.ps1 -Version 0.3.4
#>
[CmdletBinding()]
param([string]$Version = '', [string]$Path = '')

function Get-SshoreArchitecture {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'This installer supports Windows only; use install.sh on Linux.'
    }
    if ([Environment]::OSVersion.Version.Build -lt 22000) {
        throw 'Windows 11 / Server 2025 or newer required.'
    }
    # OS architecture, not the bitness of the PowerShell process. The fallback
    # covers Windows PowerShell hosts without RuntimeInformation available.
    try { $architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() }
    catch {
        $architecture = $env:PROCESSOR_ARCHITEW6432
        if (-not $architecture) { $architecture = $env:PROCESSOR_ARCHITECTURE }
    }
    switch ($architecture) {
        { $_ -in 'X64', 'AMD64' } { return 'amd64' }
        default { throw "Unsupported Windows architecture: $architecture (requires amd64; ARM is not supported)." }
    }
}

function Receive-SshoreFile([uri]$Uri, [string]$Destination, [long]$Limit = [int]::MaxValue) {
    Add-Type -AssemblyName System.Net.Http
    $oldTls = [Net.ServicePointManager]::SecurityProtocol
    $handler = New-Object Net.Http.HttpClientHandler
    $handler.AllowAutoRedirect = $false
    $client = New-Object Net.Http.HttpClient($handler)
    $client.MaxResponseContentBufferSize = $Limit
    $client.Timeout = [TimeSpan]::FromSeconds(180)
    try {
        [Net.ServicePointManager]::SecurityProtocol = $oldTls -bor [Net.SecurityProtocolType]::Tls12
        for ($redirect = 0; $redirect -le 10; $redirect++) {
            if (-not $Uri.IsAbsoluteUri -or $Uri.Scheme -ne 'https') {
                throw 'Downloads and redirects must use HTTPS.'
            }
            $response = $client.GetAsync($Uri).GetAwaiter().GetResult()
            try {
                if ([int]$response.StatusCode -in 301, 302, 303, 307, 308) {
                    if (-not $response.Headers.Location) { throw 'Redirect has no Location header.' }
                    $Uri = New-Object Uri($Uri, $response.Headers.Location)
                    continue
                }
                $null = $response.EnsureSuccessStatusCode()
                $stream = [IO.File]::Create($Destination)
                try { $null = $response.Content.CopyToAsync($stream).GetAwaiter().GetResult() }
                finally { $stream.Dispose() }
                return
            } finally { $response.Dispose() }
        }
        throw 'Too many download redirects.'
    } finally {
        $client.Dispose()
        $handler.Dispose()
        [Net.ServicePointManager]::SecurityProtocol = $oldTls
    }
}

function Read-SshoreBinaryVersion([string]$Path) {
    $lines = @(& $Path --version 2>$null)
    if ($LASTEXITCODE -ne 0) { throw "Version check failed: $Path" }
    return ($lines -join "`n")
}

function Get-SshoreSha256([string]$Path) {
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        $stream = [IO.File]::OpenRead($Path)
        try { return [BitConverter]::ToString($sha256.ComputeHash($stream)).Replace('-', '') }
        finally { $stream.Dispose() }
    } finally { $sha256.Dispose() }
}

function Get-SshoreInstallDirectory([ref]$FoundOnPath) {
    $command = Get-Command sshore.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($FoundOnPath) { $FoundOnPath.Value = [bool]$command }
    if ($command) { return [IO.Path]::GetDirectoryName($command.Path) }
    $localAppData = [Environment]::GetFolderPath('LocalApplicationData')
    if (-not $localAppData) { throw 'Cannot find LocalAppData.' }
    return Join-Path $localAppData 'sshore\bin'
}

function Add-SshorePathDirectory([string]$Path, [string]$Directory) {
    $normalized = $Directory.Replace('/', '\').TrimEnd('\')
    foreach ($entry in ($Path -split ';')) {
        $expanded = [Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"'))
        if ($expanded.Replace('/', '\').TrimEnd('\') -ieq $normalized) { return $Path }
    }
    if ([string]::IsNullOrEmpty($Path)) { return $Directory }
    return $Path.TrimEnd(';') + ';' + $Directory
}

function Install-Sshore([string]$Version = '', [string]$Path = '') {
    $ErrorActionPreference = 'Stop'
    $versionPattern = '(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
    if ($Version -and $Version -cnotmatch "\A$versionPattern\z") {
        throw 'Version must be X.Y.Z (without v).'
    }
    $architecture = Get-SshoreArchitecture
    $foundOnPath = $false
    if ($Path) {
        if ($Path -notmatch '\A([A-Za-z]:[\\/]|\\\\)') { throw 'Installation path must be absolute.' }
        $target = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
        $directory = [IO.Path]::GetDirectoryName($target)
    } else {
        $directory = Get-SshoreInstallDirectory ([ref]$foundOnPath)
        $directory = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($directory)
        $target = Join-Path $directory 'sshore.exe'
    }
    $existing = Get-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
    if ($existing -and ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint))) {
        throw 'Installation target is a directory or reparse point; installation unchanged.'
    }
    $temp = Join-Path ([IO.Path]::GetTempPath()) ('sshore-install-' + [Guid]::NewGuid().ToString('N'))
    $null = [IO.Directory]::CreateDirectory($temp)
    try {
        $releases = 'https://github.com/jay-waves/sshore/releases'
        $manifestUrl = "$releases/latest/download/SHA256SUMS"
        if ($Version) { $manifestUrl = "$releases/download/v$Version/SHA256SUMS" }
        $manifest = Join-Path $temp 'SHA256SUMS'
        Receive-SshoreFile $manifestUrl $manifest
        $entries = @(
            foreach ($line in [IO.File]::ReadAllLines($manifest)) {
                if ($line -cmatch "\A(?<hash>[0-9a-fA-F]{64})[ `t]+\*?(?<asset>sshore_(?<version>$versionPattern)_windows_(?<arch>amd64)\.zip)\z") {
                    if ($Matches.arch -eq $architecture -and (-not $Version -or $Matches.version -eq $Version)) {
                        [pscustomobject]@{ Hash = $Matches.hash; Asset = $Matches.asset; Version = $Matches.version }
                    }
                }
            }
        )
        if ($entries.Count -ne 1) { throw 'Missing, invalid, or duplicate Windows checksum entry.' }
        $entry = $entries[0]
        $Version = $entry.Version
        $archivePath = Join-Path $temp 'release.zip'
        Receive-SshoreFile "$releases/download/v$Version/$($entry.Asset)" $archivePath
        if ((Get-SshoreSha256 $archivePath) -ine $entry.Hash) {
            throw 'SHA256 mismatch; installation unchanged.'
        }
        # Windows PowerShell 5.1 does not automatically load the ZIP core assembly.
        Add-Type -AssemblyName System.IO.Compression
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $archive = [IO.Compression.ZipFile]::OpenRead($archivePath)
        $payload = Join-Path $temp 'sshore.exe'
        try {
            # Never extract archive paths: copy only one exact root executable.
            $executables = @($archive.Entries | Where-Object { $_.FullName -ceq 'sshore.exe' })
            if ($executables.Count -ne 1) { throw 'Release ZIP must contain exactly one root sshore.exe.' }
            $source = $executables[0].Open()
            try {
                $output = [IO.File]::Create($payload)
                try { $source.CopyTo($output) } finally { $output.Dispose() }
            } finally { $source.Dispose() }
        } finally { $archive.Dispose() }

        $null = [IO.Directory]::CreateDirectory($directory)
        $existing = Get-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
        if ($existing -and ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint))) {
            throw 'Installation target is a directory or reparse point; installation unchanged.'
        }
        if ((Read-SshoreBinaryVersion $payload) -cne "sshore $Version") {
            throw 'Binary version does not match release; installation unchanged.'
        }
        # Older published targets expose the same apply operation as bootstrap.
        $commands = & $payload --help
        if ($LASTEXITCODE -ne 0) { throw 'Could not read the staged client help.' }
        $action = if ($commands -match '^\s+install\s') { 'install' } else { 'bootstrap' }
        & $payload $action --apply "here:$target"
        if ($LASTEXITCODE -ne 0) { throw "Installation stopped (installer exit code $LASTEXITCODE). See the error above." }
        Write-Host "Installed sshore ${Version}: $target"
        if ($Path -or -not $foundOnPath) {
            try {
                $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
                $updatedPath = Add-SshorePathDirectory $userPath $directory
                if ($updatedPath -cne $userPath) {
                    [Environment]::SetEnvironmentVariable('Path', $updatedPath, 'User')
                }
                $env:Path = Add-SshorePathDirectory $env:Path $directory
            } catch {
                throw "Installed successfully, but PATH setup failed. Add '$directory' to your user PATH manually. $($_.Exception.Message)"
            }
            Write-Host 'Added to user and current session PATH. Restart other terminals to use sshore.'
        }
        Write-Host 'SHA256 verified. Managed SSH connections deploy the matching server automatically.'

    } finally {
        [IO.Directory]::Delete($temp, $true)
    }
}

# Dot-sourcing loads helpers for offline tests.
if ($MyInvocation.InvocationName -ne '.') {
    Install-Sshore -Version $Version -Path $Path
}
