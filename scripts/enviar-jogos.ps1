# Envia os jogos de assets/games para um servidor Belko Arcade pela API de administração.
# Uso:  .\scripts\enviar-jogos.ps1 -Url https://seu-dominio -Email wenderson@belko.com.br
# (a senha é pedida no terminal; arquivos que já existem no servidor são pulados)
param(
    [Parameter(Mandatory = $true)][string]$Url,
    [Parameter(Mandatory = $true)][string]$Email,
    [string]$Games = (Join-Path $PSScriptRoot '..\assets\games')
)
$ErrorActionPreference = 'Stop'
$Url = $Url.TrimEnd('/')
$jar = Join-Path $env:TEMP 'belko-upload-cookies.txt'
$body = Join-Path $env:TEMP 'belko-login.json'
Remove-Item $jar, $body -ErrorAction SilentlyContinue

$sec = Read-Host 'Senha do admin' -AsSecureString
$pass = [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec))
$json = @{ email = $Email; password = $pass } | ConvertTo-Json -Compress
# sem BOM: o servidor recusa JSON com BOM
[IO.File]::WriteAllText($body, $json, (New-Object Text.UTF8Encoding($false)))
$code = curl.exe -s -o NUL -w '%{http_code}' -c $jar -X POST -H 'Content-Type: application/json' --data-binary "@$body" "$Url/api/auth/login"
Remove-Item $body
if ($code -ne '200') { throw "Login falhou (HTTP $code)." }

# extensão -> sistema (pasta nes/ guarda ROMs de SNES; o sistema é decidido pela extensão)
$snes = 'smc', 'sfc', 'swc', 'fig', 'bs'
$ps1 = 'cue', 'bin', 'chd', 'iso', 'img', 'pbp', 'm3u', 'sub', 'ccd'
$files = Get-ChildItem $Games -Recurse -File | ForEach-Object {
    $ext = $_.Extension.TrimStart('.').ToLower()
    $folder = $_.Directory.Name
    $system = $null
    if ($snes -contains $ext) { $system = 'snes' }
    elseif ($folder -eq 'psx' -and $ps1 -contains $ext) { $system = 'pcsx' }
    elseif ($ext -eq 'nes') { $system = 'nes' }
    if ($system) { [pscustomobject]@{ File = $_; System = $system } }
}
# .cue antes dos .bin
$files = $files | Sort-Object { if ($_.File.Extension -ieq '.cue') { 0 } else { 1 } }, { $_.File.Name }

$chunk = 64MB
$ok = 0; $skip = 0; $fail = 0; $i = 0
foreach ($f in $files) {
    $i++
    $name = [uri]::EscapeDataString($f.File.Name)
    $q = "name=$name&system=$($f.System)"
    $mb = [math]::Round($f.File.Length / 1MB, 1)
    Write-Host ("[{0}/{1}] {2} ({3} MB) " -f $i, $files.Count, $f.File.Name, $mb) -NoNewline
    $out = Join-Path $env:TEMP 'belko-upload-resp.txt'
    $code = '000'
    if ($f.File.Length -le $chunk) {
        $code = curl.exe -s -o $out -w '%{http_code}' -b $jar -X PUT -H 'Content-Type: application/octet-stream' -T $f.File.FullName "$Url/api/admin/games?$q"
    } else {
        # arquivo grande: pedaços de 64 MB (o proxy derruba envios longos), cada um com até 3 tentativas
        $tmp = Join-Path $env:TEMP 'belko-upload-chunk.bin'
        $fs = [IO.File]::OpenRead($f.File.FullName)
        try {
            $buf = New-Object byte[] $chunk
            $offset = 0L
            while ($offset -lt $f.File.Length) {
                $fs.Position = $offset
                $n = $fs.Read($buf, 0, $chunk)
                $out2 = [IO.File]::Open($tmp, 'Create', 'Write')
                $out2.Write($buf, 0, $n); $out2.Close()
                for ($try = 1; $try -le 3; $try++) {
                    $code = curl.exe -s -o $out -w '%{http_code}' -b $jar -X PUT -H 'Content-Type: application/octet-stream' -T $tmp "$Url/api/admin/games?$q&offset=$offset&total=$($f.File.Length)"
                    if ($code -eq '200' -or $code -eq '409') { break }
                    Start-Sleep -Seconds 2
                }
                if ($code -eq '409') { $code = 'fora de ordem' }
                if ($code -ne '200') { break }
                $offset += $n
                Write-Host ('{0}% ' -f [int](100 * $offset / $f.File.Length)) -NoNewline
            }
        } finally { $fs.Close(); Remove-Item $tmp -ErrorAction SilentlyContinue }
    }
    switch ($code) {
        '200' { $ok++; Write-Host 'enviado' -ForegroundColor Green }
        '409' { $skip++; Write-Host 'já existe, pulado' -ForegroundColor Yellow }
        default { $fail++; Write-Host "ERRO $code $(Get-Content $out -Raw)" -ForegroundColor Red }
    }
}
curl.exe -s -o NUL -b $jar -X POST -H 'Content-Type: application/json' "$Url/api/admin/games/rescan" | Out-Null
Remove-Item $jar -ErrorAction SilentlyContinue
Write-Host "Pronto: $ok enviados, $skip pulados, $fail com erro. Recarregue a página do arcade."
