param([string]$Version="dev")
$ErrorActionPreference="Stop"
$env:CGO_ENABLED="1"
$goos=$env:GOOS; if(-not $goos){$goos="windows"}
$ext=if($goos -eq "windows"){".dll"}elseif($goos -eq "darwin"){".dylib"}else{".so"}
$out="cpa-usage-manager$ext"
go build -buildmode=c-shared -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=$Version" -o $out .
Write-Host "built $out"
