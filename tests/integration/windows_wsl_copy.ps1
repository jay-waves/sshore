$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

Write-Host 'WSL distributions registered on the runner before fixture setup:'
& {
    $PSNativeCommandUseErrorActionPreference = $false
    wsl.exe --list --verbose
}

$rootfs = Join-Path $env:RUNNER_TEMP 'alpine-minirootfs-3.22.0-x86_64.tar.gz'
$url = 'https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/x86_64'
Invoke-WebRequest "$url/alpine-minirootfs-3.22.0-x86_64.tar.gz" -OutFile $rootfs
if ((Get-FileHash $rootfs -Algorithm SHA256).Hash -ne '18879884e35b0718f017a50ff85b5e6568279e97233fc42822229585feb2fa4d') {
    throw 'Alpine rootfs checksum mismatch'
}

$distro = 'sshore-copy-fixture'
$location = Join-Path $env:RUNNER_TEMP $distro
# WSL 1 shares the runner's loopback, so the fixture sshd stays bound there.
wsl.exe --import $distro $location $rootfs --version 1

try {
    $builder = Join-Path $env:RUNNER_TEMP 'sshore-build.exe'
    $client = Join-Path $env:RUNNER_TEMP 'sshore-wsl'
    $assetServer = Join-Path $env:RUNNER_TEMP 'sshore-release-server'
    $assets = Join-Path $env:RUNNER_TEMP 'sshore-wsl-releases'
    $release = Join-Path $assets 'v0.0.0'
    New-Item -ItemType Directory -Path $release | Out-Null
    $assetName = 'sshore_0.0.0_windows_amd64.zip'
    $archive = Join-Path $release $assetName
    Compress-Archive -LiteralPath $env:SSHORE_TEST_WINDOWS_CLIENT -DestinationPath $archive
    $digest = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -Encoding ascii (Join-Path $release 'SHA256SUMS') "$digest  $assetName"
    go build -o $builder ./scripts
    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    & $builder go build -ldflags '-X main.version=0.0.0 -X main.deploymentReleaseBase=http://127.0.0.1:22221' -o $client .
    go build -o $assetServer (Join-Path $PSScriptRoot 'testdata/release_server.go')


    $paths = @(
        (Join-Path $PSScriptRoot 'windows_wsl_copy.sh')
        $client
        $env:SSHORE_TEST_WINDOWS_SSH_KEY
        $env:SSHORE_TEST_WINDOWS_SSH_HOSTS
        $assetServer
        $assets
    ) | ForEach-Object { (wsl.exe -d $distro --exec wslpath -a $_).Trim() }
    wsl.exe -d $distro --exec /bin/sh @paths
} finally {
    wsl.exe --unregister $distro
}
