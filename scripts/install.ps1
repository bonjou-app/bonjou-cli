# Bonjou installer for Windows.
#
#   iwr https://raw.githubusercontent.com/kodolabs-hq/bonjou-cli/main/scripts/install.ps1 -useb | iex
#
# Tries WinGet, then Scoop, then a direct download. Every path reports what
# actually failed rather than surfacing a raw .NET exception, because the
# usual outcome of one of those is that somebody gives up.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Repo = 'kodolabs-hq/bonjou-cli'
$WinGetId = 'HamzaAbdulWahab.Bonjou'
$ScoopManifestUrl = 'https://raw.githubusercontent.com/hamzaabdulwahab/scoop-bonjou/main/bonjou.json'

# Windows PowerShell 5.1 still negotiates TLS 1.0 or 1.1 by default, and
# GitHub has required 1.2 since 2018. Without this the very first web call
# dies with "Could not create SSL/TLS secure channel", which reads like a
# certificate problem and is not one.
try {
    $protocols = [Net.ServicePointManager]::SecurityProtocol
    if ($protocols -notmatch 'Tls12') {
        [Net.ServicePointManager]::SecurityProtocol = $protocols -bor [Net.SecurityProtocolType]::Tls12
    }
} catch {
    Write-Warning "Could not raise the TLS version; downloads may fail on older Windows. $($_.Exception.Message)"
}

function Write-Info {
    param([string]$Message)
    Write-Host $Message
}

function Write-Failure {
    param([string]$Message)
    Write-Host ''
    Write-Host "Install failed: $Message" -ForegroundColor Red
    Write-Host ''
    Write-Host 'You can install manually instead:'
    Write-Host "  1. Download bonjou.exe from https://github.com/$Repo/releases/latest"
    Write-Host '  2. Put it in a folder such as %LOCALAPPDATA%\Programs\Bonjou'
    Write-Host '  3. Add that folder to your PATH'
    Write-Host ''
    Write-Host "If this keeps happening, please report it with the message above:"
    Write-Host "  https://github.com/$Repo/issues"
}

function Get-LatestVersion {
    $api = "https://api.github.com/repos/$Repo/releases/latest"
    try {
        # -UseBasicParsing matters on Server Core and hardened images, where
        # the Internet Explorer engine Invoke-* reaches for is absent.
        $release = Invoke-RestMethod -Uri $api -UseBasicParsing -Headers @{
            'User-Agent' = 'bonjou-installer'
        }
    } catch {
        throw "could not reach the GitHub API ($($_.Exception.Message))"
    }
    if (-not $release.tag_name) {
        throw 'GitHub returned a release with no version tag.'
    }
    return ($release.tag_name -replace '^v', '')
}

function Add-ToUserPathIfMissing {
    param([string]$Dir)

    $currentPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ([string]::IsNullOrWhiteSpace($currentPath)) {
        [Environment]::SetEnvironmentVariable('Path', $Dir, 'User')
    } else {
        $parts = $currentPath.Split(';')
        if ($parts -notcontains $Dir) {
            [Environment]::SetEnvironmentVariable('Path', "$currentPath;$Dir", 'User')
        }
    }

    # The stored PATH only reaches new processes, so make the command work
    # in this window too rather than telling the user to reopen it.
    if (($env:Path -split ';') -notcontains $Dir) {
        $env:Path = "$env:Path;$Dir"
    }
}

function Install-Direct {
    $version = Get-LatestVersion
    $url = "https://github.com/$Repo/releases/download/v$version/bonjou.exe"

    $installDir = Join-Path $env:LOCALAPPDATA 'Programs\Bonjou'
    $target = Join-Path $installDir 'bonjou.exe'
    $temp = "$target.download"

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Write-Info "Downloading Bonjou v$version..."
    try {
        Invoke-WebRequest -Uri $url -OutFile $temp -UseBasicParsing
    } catch {
        if (Test-Path $temp) { Remove-Item $temp -Force }
        throw "download failed ($($_.Exception.Message))"
    }

    # A truncated or error-page download would otherwise be installed and
    # then fail at run time, far from the actual cause.
    $size = (Get-Item $temp).Length
    if ($size -lt 1MB) {
        Remove-Item $temp -Force
        throw "the downloaded file is only $size bytes, which is not the Bonjou binary."
    }
    $header = [System.IO.File]::ReadAllBytes($temp)[0..1]
    if ($header[0] -ne 0x4D -or $header[1] -ne 0x5A) {
        Remove-Item $temp -Force
        throw 'the downloaded file is not a Windows executable.'
    }

    Move-Item -Path $temp -Destination $target -Force
    Add-ToUserPathIfMissing -Dir $installDir
    Write-Info "Installed to $target"
    return $true
}

function Confirm-Install {
    try {
        $reported = & bonjou --version 2>&1
        Write-Info ''
        Write-Info "Bonjou is ready: $reported"
        Write-Info 'Run it with:  bonjou'
        return $true
    } catch {
        Write-Info ''
        Write-Info 'Installed, but the command is not on PATH in this window yet.'
        Write-Info 'Open a new terminal and run:  bonjou'
        return $false
    }
}

try {
    if (Get-Command winget -ErrorAction SilentlyContinue) {
        Write-Info 'WinGet found. Installing via WinGet...'
        winget install --id $WinGetId --exact --silent --accept-package-agreements --accept-source-agreements
        if ($LASTEXITCODE -eq 0) {
            Write-Info 'Installed via WinGet.'
            Confirm-Install | Out-Null
            exit 0
        }
        Write-Info 'WinGet did not have the package. Trying Scoop...'
    }

    if (Get-Command scoop -ErrorAction SilentlyContinue) {
        Write-Info 'Scoop found. Installing via manifest...'
        scoop install $ScoopManifestUrl
        if ($LASTEXITCODE -eq 0) {
            Write-Info 'Installed via Scoop.'
            Confirm-Install | Out-Null
            exit 0
        }
        # Previously a Scoop failure ended the script silently, leaving
        # nothing installed and nothing said.
        Write-Info 'Scoop could not install it. Downloading directly...'
    }

    Install-Direct | Out-Null
    Confirm-Install | Out-Null
    exit 0
} catch {
    Write-Failure $_.Exception.Message
    exit 1
}
