$ErrorActionPreference = 'Stop'

$packageName = 'bonjou'
$toolsDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$exePath = Join-Path $toolsDir 'bonjou.exe'
$url64 = 'https://github.com/kodolabs-hq/bonjou-cli/releases/download/v1.1.0/bonjou.exe'
$checksum64 = 'c3c6dd5fabf8f03128a92289547a3e8e4b74c46254497052f26349d0106210b6'

Get-ChocolateyWebFile `
  -PackageName $packageName `
  -FileFullPath $exePath `
  -Url64bit $url64 `
  -Checksum64 $checksum64 `
  -ChecksumType64 'sha256'

Install-BinFile -Name 'bonjou' -Path $exePath