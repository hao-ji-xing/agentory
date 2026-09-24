# Install agentory on Windows from a GitHub release.
#
#   irm https://raw.githubusercontent.com/hao-ji-xing/agentory/main/install.ps1 | iex
#
# Downloads the zip for this CPU, verifies it against the release's
# checksums.txt, puts agentory.exe in %LOCALAPPDATA%\Programs\agentory, adds
# that directory to the user PATH, and installs the bundled agent skill for
# Claude Code and Codex when they are present.
#
# Environment variables:
#   AGENTORY_VERSION       release to install, e.g. v0.2.0 (default: latest)
#   AGENTORY_INSTALL_DIR   where to put agentory.exe
#   AGENTORY_SKILL=no      do not install the agent skill
#   AGENTORY_DOWNLOAD_BASE overrides https://github.com/<repo>/releases/download

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'hao-ji-xing/agentory'
$version = if ($env:AGENTORY_VERSION) { $env:AGENTORY_VERSION } else { 'latest' }
$installDir = if ($env:AGENTORY_INSTALL_DIR) { $env:AGENTORY_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\agentory' }
$base = if ($env:AGENTORY_DOWNLOAD_BASE) { $env:AGENTORY_DOWNLOAD_BASE } else { "https://github.com/$repo/releases/download" }

if ($version -eq 'latest') {
    $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
    if (-not $version) { throw "no release found for $repo" }
}
if (-not $version.StartsWith('v')) { $version = "v$version" }

$arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
    'Arm64' { 'arm64' }
    'X64'   { 'amd64' }
    default { throw "unsupported CPU: $_" }
}
$asset = "agentory_$($version.TrimStart('v'))_windows_$arch.zip"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("agentory-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading agentory $version for windows/$arch"
    Invoke-WebRequest "$base/$version/$asset" -OutFile (Join-Path $tmp $asset)
    Invoke-WebRequest "$base/$version/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { ($_ -split '\s+')[1] -eq $asset }
    if (-not $line) { throw "$asset is not listed in checksums.txt" }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash (Join-Path $tmp $asset) -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $want) { throw "checksum mismatch for $asset (got $got, want $want)" }

    $x = Join-Path $tmp 'x'
    Expand-Archive (Join-Path $tmp $asset) -DestinationPath $x
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    Copy-Item (Join-Path $x 'agentory.exe') (Join-Path $installDir 'agentory.exe') -Force
    Write-Host "Installed $(Join-Path $installDir 'agentory.exe')"

    # The skill tells an agent when and how to use agentory.
    # A skill added with `npx skills add` lives in ~/.agents/skills and is kept
    # up to date by `npx skills update`; a second copy would show up twice.
    $skillSrc = Join-Path $x 'skills\agentory'
    $managed = Join-Path $HOME '.agents\skills\agentory'
    if ($env:AGENTORY_SKILL -ne 'no' -and (Test-Path $managed)) {
        Write-Host "Skill managed by 'npx skills' ($managed), left as is"
    } elseif ($env:AGENTORY_SKILL -ne 'no' -and (Test-Path $skillSrc)) {
        $claude = if ($env:CLAUDE_CONFIG_DIR) { $env:CLAUDE_CONFIG_DIR } else { Join-Path $HOME '.claude' }
        $codex = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME '.codex' }
        foreach ($agent in @(@($claude, 'Claude Code'), @($codex, 'Codex'))) {
            if (-not (Test-Path $agent[0])) { continue }
            $dest = Join-Path $agent[0] 'skills\agentory'
            $item = Get-Item $dest -ErrorAction SilentlyContinue
            if ($item -and $item.LinkType) { Write-Host "Skill for $($agent[1]) is a link ($dest), left as is"; continue }
            New-Item -ItemType Directory -Force -Path $dest | Out-Null
            Copy-Item (Join-Path $skillSrc '*') $dest -Recurse -Force
            Write-Host "Installed the $($agent[1]) skill in $dest"
        }
    }
} finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($userPath -split ';') -contains $installDir)) {
    [Environment]::SetEnvironmentVariable('Path', "$installDir;$userPath", 'User')
    Write-Host "Added $installDir to your user PATH; open a new terminal to use it."
}
Write-Host "Next: run 'agentory doctor', then 'agentory index' to build the index."
