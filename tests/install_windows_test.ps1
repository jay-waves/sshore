# Offline installer regression tests; no Pester dependency or real release downloads.
# Windows CI runs this with both Windows PowerShell 5.1 and PowerShell 7.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\install.ps1')
$root = Join-Path ([IO.Path]::GetTempPath()) ('sshore-installer-test-' + [Guid]::NewGuid().ToString('N'))
$oldInstallDir = $env:INSTALL_DIR
$oldPath = $env:PATH
$pathTargets = @('Process')
if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) { $pathTargets += 'User', 'Machine' }
$paths = @{}
foreach ($scope in $pathTargets) { $paths[$scope] = [Environment]::GetEnvironmentVariable('Path', $scope) }
$script:TestArchitecture = 'amd64'
$script:Downloads = @()

function Assert-True($Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}
function Assert-Fails([scriptblock]$Action, [string]$Message) {
    try { & $Action } catch {
        Assert-True ($_.Exception.Message -like "*$Message*") "Unexpected failure: $_"
        return
    }
    throw "Expected failure: $Message"
}
function Get-SshoreArchitecture { return $script:TestArchitecture }
function Receive-SshoreFile([uri]$Uri, [string]$Destination) {
    Assert-True ($Uri.Scheme -eq 'https') 'Installer requested non-HTTPS URL'
    $script:Downloads += $Uri.AbsoluteUri
    [IO.File]::Copy((Join-Path $root $Uri.Segments[-1]), $Destination, $true)
}
# Allow the same offline tests to run on a Linux development machine with pwsh.
# Production architecture detection rejects Linux; the fixture is a native binary.
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    $script:VersionReader = ${function:Read-SshoreBinaryVersion}
    function Read-SshoreBinaryVersion([string]$Path) {
        & chmod +x $Path
        & $script:VersionReader $Path
    }
}
function New-TestRelease([string]$ReleaseVersion, [string]$Architecture, [string]$EntryName = 'sshore.exe') {
    $asset = "sshore_${ReleaseVersion}_windows_${Architecture}.zip"
    $archivePath = Join-Path $root $asset
    if ([IO.File]::Exists($archivePath)) { [IO.File]::Delete($archivePath) }
    $zip = [IO.Compression.ZipFile]::Open($archivePath, [IO.Compression.ZipArchiveMode]::Create)
    try { $null = [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $fixture, $EntryName) }
    finally { $zip.Dispose() }
    $hash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash
    [IO.File]::WriteAllText((Join-Path $root 'SHA256SUMS'), "$hash  $asset`r`n")
    return $archivePath
}

try {
    $null = [IO.Directory]::CreateDirectory($root)
    # Windows PowerShell 5.1 does not automatically load the ZIP core assembly.
    Add-Type -AssemblyName System.IO.Compression
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $source = Join-Path $root 'fixture.go'
    $fixture = Join-Path $root 'fixture.exe'
    [IO.File]::WriteAllText($source, 'package main; import "fmt"; func main() { fmt.Println("sshore 0.3.3") }')
    & go build -o $fixture $source
    if ($LASTEXITCODE -ne 0) { throw 'Cannot build installer fixture' }
    $env:INSTALL_DIR = Join-Path $root "directory with spaces and ' quote"
    $null = [IO.Directory]::CreateDirectory($env:INSTALL_DIR)
    $target = Join-Path $env:INSTALL_DIR 'sshore.exe'

    # Successful install/upgrade, pinned and latest URL selection, custom paths,
    # and PATH remaining unchanged are the user-visible installation contract.
    $null = New-TestRelease '0.3.3' 'amd64'
    [IO.File]::WriteAllText($target, 'previous binary')
    Install-Sshore -Version '0.3.3'
    Assert-True ((Read-SshoreBinaryVersion $target) -eq 'sshore 0.3.3') 'Wrong installed binary'
    Assert-True ($script:Downloads[0] -like '*/download/v0.3.3/SHA256SUMS') 'Pinned URL not used'
    $script:TestArchitecture = 'arm64'
    $script:Downloads = @()
    $archivePath = New-TestRelease '0.3.3' 'arm64'
    Install-Sshore
    Assert-True ($script:Downloads[0] -like '*/latest/download/SHA256SUMS') 'Latest URL not used'
    Assert-True ($script:Downloads[1] -like '*/download/v0.3.3/*_windows_arm64.zip') 'Wrong architecture/version URL'
    foreach ($scope in $pathTargets) {
        Assert-True ([Environment]::GetEnvironmentVariable('Path', $scope) -ceq $paths[$scope]) "Changed $scope PATH"
    }
    Write-Host 'PASS: install/upgrade, release selection, INSTALL_DIR and unchanged PATH'

    # Neither corrupted downloads nor malformed/mislabeled packages may replace
    # the existing executable. Verify actual contents, not implementation calls.
    $installedHash = (Get-FileHash -LiteralPath $target).Hash
    $stream = [IO.File]::OpenWrite($archivePath)
    try { $null = $stream.Seek(0, [IO.SeekOrigin]::End); $stream.WriteByte(1) } finally { $stream.Dispose() }
    Assert-Fails { Install-Sshore -Version '0.3.3' } 'SHA256 mismatch'
    $null = New-TestRelease '0.3.3' 'arm64' '../sshore.exe'
    Assert-Fails { Install-Sshore -Version '0.3.3' } 'exactly one root sshore.exe'
    $null = New-TestRelease '0.3.4' 'arm64'
    Assert-Fails { Install-Sshore -Version '0.3.4' } 'Binary version does not match'
    Assert-True ((Get-FileHash -LiteralPath $target).Hash -eq $installedHash) 'Existing binary changed on failure'
    Assert-True (@(Get-ChildItem -LiteralPath $env:INSTALL_DIR -Force).Count -eq 1) 'Installer left staging files'
    Write-Host 'PASS: failed verification preserves the existing executable'

    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $null = New-TestRelease '0.3.3' 'arm64'
        $locked = [IO.File]::Open($target, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        try {
            $failed = $false
            try { Install-Sshore -Version '0.3.3' } catch { $failed = $true }
            Assert-True $failed 'Locked target was replaced'
        } finally { $locked.Dispose() }
        Assert-True ((Get-FileHash -LiteralPath $target).Hash -eq $installedHash) 'Locked target was damaged'
        Write-Host 'PASS: locked executable fails without removing the installed version'
    }
} finally {
    $env:INSTALL_DIR = $oldInstallDir
    $env:PATH = $oldPath
    if ([IO.Directory]::Exists($root)) { [IO.Directory]::Delete($root, $true) }
}
