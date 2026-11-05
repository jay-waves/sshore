#requires -Version 5.1
<#
.SYNOPSIS
Install the Windows sshore client without changing PATH or requiring elevation.
.PARAMETER Version
Published X.Y.Z version (without v). Defaults to the latest release.
.EXAMPLE
.\install.ps1 -Version 0.3.4
.EXAMPLE
$env:INSTALL_DIR = 'D:\Tools\sshore'; .\install.ps1
#>
[CmdletBinding()]
param([string]$Version = '')

function Get-SshoreArchitecture {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'This installer supports Windows only; use install.sh on Linux.'
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

function Receive-SshoreFile([uri]$Uri, [string]$Destination) {
    Add-Type -AssemblyName System.Net.Http
    $oldTls = [Net.ServicePointManager]::SecurityProtocol
    $handler = New-Object Net.Http.HttpClientHandler
    $handler.AllowAutoRedirect = $false
    $client = New-Object Net.Http.HttpClient($handler)
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

function Install-Sshore([string]$Version = '') {
    $ErrorActionPreference = 'Stop'
    $versionPattern = '(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
    if ($Version -and $Version -cnotmatch "\A$versionPattern\z") {
        throw 'Version must be X.Y.Z (without v).'
    }
    $architecture = Get-SshoreArchitecture
    $directory = $env:INSTALL_DIR
    if ([string]::IsNullOrWhiteSpace($directory)) {
        $localAppData = [Environment]::GetFolderPath('LocalApplicationData')
        if (-not $localAppData) { throw 'Cannot find LocalAppData; set INSTALL_DIR explicitly.' }
        $directory = Join-Path $localAppData 'sshore\bin'
    }
    # Resolve relative filesystem paths using PowerShell's current location.
    $directory = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($directory)
    $temp = Join-Path ([IO.Path]::GetTempPath()) ('sshore-install-' + [Guid]::NewGuid().ToString('N'))
    $stage = $null
    $rollback = $null
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
        if ((Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash -ine $entry.Hash) {
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
        $target = Join-Path $directory 'sshore.exe'
        $existing = Get-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
        if ($existing -and ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint))) {
            throw 'Installation target is a directory or reparse point; installation unchanged.'
        }
        $stage = Join-Path $directory ('.sshore-install-' + [Guid]::NewGuid().ToString('N') + '.exe')
        [IO.File]::Copy($payload, $stage, $false)
        if ((Read-SshoreBinaryVersion $stage) -cne "sshore $Version") {
            throw 'Binary version does not match release; installation unchanged.'
        }
        if ($existing) {
            $rollback = Join-Path $directory ('.sshore-previous-' + [Guid]::NewGuid().ToString('N') + '.exe')
            # Same-volume replacement; no delete-then-copy fallback. A locked
            # executable or unsupported filesystem fails without removing it.
            [IO.File]::Replace($stage, $target, $rollback)
        } else {
            [IO.File]::Move($stage, $target)
        }
        $stage = $null
        if ($rollback) { [IO.File]::Delete($rollback); $rollback = $null }
        Write-Host "Installed sshore ${Version}: $target"
        Write-Host 'SHA256 verified. PATH, PowerShell profiles and execution policy were not changed.'
        Write-Host 'Use matching client/server versions. Existing remote daemons are not upgraded or stopped.'
        $quotedDirectory = $directory.Replace("'", "''")
        $quotedTarget = $target.Replace("'", "''")
        Write-Host "Run directly: & '$quotedTarget' --version"
        Write-Host "Optional PATH setup for this PowerShell session (not executed):"
        Write-Host ('$env:Path = ''' + $quotedDirectory + ';'' + $env:Path')
        Write-Host "For persistent access, manually add '$directory' to your user Path in Windows Environment Variables."
    } finally {
        if ($stage -and [IO.File]::Exists($stage)) { [IO.File]::Delete($stage) }
        if ($rollback -and [IO.File]::Exists($rollback)) { Write-Warning "Previous binary preserved at $rollback; recover it manually if needed." }
        [IO.Directory]::Delete($temp, $true)
    }
}

# Dot-sourcing loads helpers for offline tests; normal script/scriptblock use installs.
if ($MyInvocation.InvocationName -ne '.') { Install-Sshore -Version $Version }
