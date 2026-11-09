# Installs the gitopsify binary on Windows from its GitHub releases. No Go needed.
#
#   irm https://raw.githubusercontent.com/hayone1/gitopsify/main/install.ps1 | iex
#
# Environment variables (all optional):
#   GITOPSIFY_VERSION      version to install, e.g. v0.2.0 (default: the latest release)
#   GITOPSIFY_INSTALL_DIR  where to put gitopsify.exe (default: %LOCALAPPDATA%\gitopsify\bin)
#   GITHUB_TOKEN           token for private repositories or to avoid API rate limits
#   GITOPSIFY_NO_MODIFY_PATH  set to 1 to leave your PATH alone
# A script block of its own: run through iex, settings stay out of the
# caller's session.
& {
$ErrorActionPreference = 'Stop'
# Windows PowerShell's progress bar makes downloads many times slower.
$ProgressPreference = 'SilentlyContinue'
$repo = 'hayone1/gitopsify'

# A 32-bit PowerShell on 64-bit Windows reports x86; the machine's is here.
$machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($machine) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "unsupported architecture $machine" }
}
$headers = @{ Accept = 'application/vnd.github+json' }
if ($env:GITHUB_TOKEN) { $headers.Authorization = "Bearer $($env:GITHUB_TOKEN)" }

$version = $env:GITOPSIFY_VERSION
if ($version) {
    if (-not $version.StartsWith('v')) { $version = "v$version" }
    $releaseUrl = "https://api.github.com/repos/$repo/releases/tags/$version"
} else {
    $releaseUrl = "https://api.github.com/repos/$repo/releases/latest"
}
$release = Invoke-RestMethod -Uri $releaseUrl -Headers $headers
$tag = $release.tag_name
$archive = "gitopsify_$($tag.TrimStart('v'))_windows_$arch.zip"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("gitopsify-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    function Get-Asset($name, $out) {
        $asset = $release.assets | Where-Object { $_.name -eq $name } | Select-Object -First 1
        if (-not $asset) { throw "release $tag has no asset $name" }
        if ($env:GITHUB_TOKEN) {
            Invoke-WebRequest -Uri $asset.url -OutFile $out -Headers @{ Accept = 'application/octet-stream'; Authorization = "Bearer $($env:GITHUB_TOKEN)" }
        } else {
            Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $out
        }
    }
    Write-Host "Downloading gitopsify $tag for windows/$arch"
    Get-Asset $archive (Join-Path $tmp $archive)
    Get-Asset 'checksums.txt' (Join-Path $tmp 'checksums.txt')

    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { $_ -match " $([regex]::Escape($archive))$" }
    if (-not $line) { throw "checksums.txt has no entry for $archive" }
    $want = ($line -split ' ')[0]
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
    if ($want -ne $got) { throw "checksum mismatch for $archive (want $want, got $got)" }

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
    $dir = $env:GITOPSIFY_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'gitopsify\bin' }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Copy-Item (Join-Path $tmp 'gitopsify.exe') (Join-Path $dir 'gitopsify.exe') -Force

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($env:GITOPSIFY_NO_MODIFY_PATH -ne '1' -and -not ($userPath -split ';' | Where-Object { $_ -eq $dir })) {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
        Write-Host "Added $dir to your user PATH; open a new terminal to use it."
    }
    Write-Host "Installed gitopsify $tag to $dir"
    Write-Host "Next: gitopsify doctor    (and gitopsify skills install, for AI coding agents)"
} finally {
    Remove-Item -Recurse -Force $tmp
}
}
