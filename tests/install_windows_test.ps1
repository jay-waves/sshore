# Offline installer regression tests; no Pester dependency or real release downloads.
# Windows CI runs this with both Windows PowerShell 5.1 and PowerShell 7.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\install.ps1')
$root = Join-Path ([IO.Path]::GetTempPath()) ('sshore-installer-test-' + [Guid]::NewGuid().ToString('N'))
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
function Get-FileHash { throw 'Get-FileHash is unavailable in this session' }
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
    $hash = (Microsoft.PowerShell.Utility\Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash
    [IO.File]::WriteAllText((Join-Path $root 'SHA256SUMS'), "$hash  $asset`r`n")
    return $archivePath
}

try {
    Assert-True ((Add-SshorePathDirectory '' 'C:\Tools') -ceq 'C:\Tools') 'Empty PATH not initialized'
    Assert-True ((Add-SshorePathDirectory 'C:\Other' 'C:\Tools') -ceq 'C:\Other;C:\Tools') 'Existing PATH not preserved'
    Assert-True ((Add-SshorePathDirectory 'C:\Other;' 'C:\Tools') -ceq 'C:\Other;C:\Tools') 'Trailing separator duplicated'
    Assert-True ((Add-SshorePathDirectory 'C:\Other;"c:/TOOLS/"' 'C:\Tools') -ceq 'C:\Other;"c:/TOOLS/"') 'Equivalent directory duplicated'
    Write-Host 'PASS: PATH addition preserves entries and avoids duplicates'
    $null = [IO.Directory]::CreateDirectory($root)
    # Windows PowerShell 5.1 does not automatically load the ZIP core assembly.
    Add-Type -AssemblyName System.IO.Compression
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $source = Join-Path $root 'fixture.go'
    $fixture = Join-Path $root 'fixture.exe'
    [IO.File]::WriteAllText($source, @'
package main
import ("fmt"; "os"; "strings")
func main() {
 if len(os.Args) == 2 && os.Args[1] == "--version" { fmt.Println("sshore 0.3.3"); return }
 if len(os.Args) != 4 || os.Args[1] != "install" || os.Args[2] != "--apply" || !strings.HasPrefix(os.Args[3], "self:") { os.Exit(2) }
 target := strings.TrimPrefix(os.Args[3], "self:")
 exe, err := os.Executable()
 if err == nil {
  var data []byte
  data, err = os.ReadFile(exe)
  if err == nil { err = os.WriteFile(target+".next", data, 0755) }
  if err == nil { err = os.Rename(target+".next", target) }
  os.Remove(target+".next")
 }
 if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
}
'@)
    & go build -o $fixture $source
    if ($LASTEXITCODE -ne 0) { throw 'Cannot build installer fixture' }
    $installDirectory = Join-Path $root "directory with spaces and ' quote"
    $null = [IO.Directory]::CreateDirectory($installDirectory)
    $target = Join-Path $installDirectory 'sshore.exe'

    # Successful install/upgrade, pinned and latest URL selection, custom paths,
    # and PATH remaining unchanged are the user-visible installation contract.
    $null = New-TestRelease '0.3.3' 'amd64'
    [IO.File]::Copy($fixture, $target)
    $env:PATH = $installDirectory + [IO.Path]::PathSeparator + $oldPath
    $paths['Process'] = $env:PATH
    Assert-True ((Get-SshoreInstallDirectory) -eq $installDirectory) 'Did not select existing PATH installation'
    Install-Sshore -Version '0.3.3'
    Assert-True ((Read-SshoreBinaryVersion $target) -eq 'sshore 0.3.3') 'Wrong installed binary'
    Assert-True ($script:Downloads[0] -like '*/download/v0.3.3/SHA256SUMS') 'Pinned URL not used'
    $script:TestArchitecture = 'amd64'
    $script:Downloads = @()
    $archivePath = New-TestRelease '0.3.3' 'amd64'
    Install-Sshore
    Assert-True ($script:Downloads[0] -like '*/latest/download/SHA256SUMS') 'Latest URL not used'
    Assert-True ($script:Downloads[1] -like '*/download/v0.3.3/*_windows_amd64.zip') 'Wrong architecture/version URL'
    foreach ($scope in $pathTargets) {
        Assert-True ([Environment]::GetEnvironmentVariable('Path', $scope) -ceq $paths[$scope]) "Changed $scope PATH"
    }
    Write-Host 'PASS: install/upgrade, release selection, PATH discovery and unchanged PATH'

    # Neither corrupted downloads nor malformed/mislabeled packages may replace
    # the existing executable. Verify actual contents, not implementation calls.
    $installedHash = (Microsoft.PowerShell.Utility\Get-FileHash -LiteralPath $target).Hash
    $stream = [IO.File]::OpenWrite($archivePath)
    try { $null = $stream.Seek(0, [IO.SeekOrigin]::End); $stream.WriteByte(1) } finally { $stream.Dispose() }
    Assert-Fails { Install-Sshore -Version '0.3.3' } 'SHA256 mismatch'
    $null = New-TestRelease '0.3.3' 'amd64' '../sshore.exe'
    Assert-Fails { Install-Sshore -Version '0.3.3' } 'exactly one root sshore.exe'
    $null = New-TestRelease '0.3.4' 'amd64'
    Assert-Fails { Install-Sshore -Version '0.3.4' } 'Binary version does not match'
    Assert-True ((Microsoft.PowerShell.Utility\Get-FileHash -LiteralPath $target).Hash -eq $installedHash) 'Existing binary changed on failure'
    Assert-True (@(Get-ChildItem -LiteralPath $installDirectory -Force).Count -eq 1) 'Installer left staging files'
    Write-Host 'PASS: failed verification preserves the existing executable'

    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $null = New-TestRelease '0.3.3' 'amd64'
        $locked = [IO.File]::Open($target, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        try {
            $downloadCount = $script:Downloads.Count
            Assert-Fails { Install-Sshore -Version '0.3.3' } 'Close remaining sshore clients'
            Assert-True ($script:Downloads.Count -eq ($downloadCount + 2)) 'Replacement was not prepared before cleanup'
        } finally { $locked.Dispose() }
        Assert-True ((Microsoft.PowerShell.Utility\Get-FileHash -LiteralPath $target).Hash -eq $installedHash) 'Locked target was damaged'
        Write-Host 'PASS: locked executable fails without removing the installed version'
    }
} finally {
    $env:PATH = $oldPath
    if ([IO.Directory]::Exists($root)) { [IO.Directory]::Delete($root, $true) }
}
