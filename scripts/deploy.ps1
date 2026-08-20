param([string]$PluginRoot="plugins",[string]$Version="dev")
$ErrorActionPreference="Stop"
$goos=$env:GOOS; if(-not $goos){$goos="windows"};$goarch=$env:GOARCH;if(-not $goarch){$goarch="amd64"}
& "$PSScriptRoot/build.ps1" -Version $Version
$ext=if($goos -eq "windows"){".dll"}elseif($goos -eq "darwin"){".dylib"}else{".so"}
$dest=Join-Path $PluginRoot (Join-Path $goos $goarch);New-Item -ItemType Directory -Force -Path $dest|Out-Null
Copy-Item -Force "cpa-usage-manager$ext" (Join-Path $dest "cpa-usage-manager$ext")
Write-Host "deployed to $dest"
