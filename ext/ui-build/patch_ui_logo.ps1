# NSM: swaps the boot/intro logo (the one that swipes in before the server list)
# inside the prebuilt CfxUI bundle for the NSM logo, and cleans display titles.
# Run by build.cmd after patch_ui_urls.ps1, once per zip.
param(
    [Parameter(Mandatory = $true)][string]$ZipPath,
    [string]$LogoPath = (Join-Path $PSScriptRoot 'nsm-logo.png')
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

if (!(Test-Path $LogoPath)) {
    Write-Error "NSM logo file not found: $LogoPath"
    exit /b 1
}
$logoBytes = [System.IO.File]::ReadAllBytes($LogoPath)
$logoB64 = [System.Convert]::ToBase64String($logoBytes)

# Boot-time swap: any <img> that points at a logo/brand asset, and any inline
# <svg> whose class/id says "logo", becomes the NSM logo. Runs repeatedly for a
# second so it also catches the swipe-intro after it mounts.
$bootScript = @"
<script data-nsm="1">
(function(){
  var L='data:image/png;base64,$logoB64';
  function s(){
    try{
      var imgs=document.querySelectorAll('img');
      for(var i=0;i<imgs.length;i++){
        var el=imgs[i], src=el.getAttribute('src')||'';
        if(/logo|brand|cfx|fivem|citizen/i.test(src)){ el.src=L; }
        var alt=el.getAttribute('alt')||'';
        if(/fivem|cfx|citizen/i.test(alt)){ el.alt='NSM'; }
      }
      var svgs=document.querySelectorAll('svg');
      for(var j=0;j<svgs.length;j++){
        var v=svgs[j], key=(v.getAttribute('class')||'')+' '+(v.getAttribute('id')||'');
        if(/logo/i.test(key) && !v.hasAttribute('data-nsm')){
          var n=document.createElement('img');
          n.src=L;
          if(v.getAttribute('class')) n.setAttribute('class', v.getAttribute('class'));
          if(v.getAttribute('style')) n.setAttribute('style', v.getAttribute('style'));
          if(v.parentNode){ v.parentNode.replaceChild(n,v); }
        }
      }
    }catch(e){}
  }
  if(document.readyState!=='loading'){ s(); } else { document.addEventListener('DOMContentLoaded', s); }
  setTimeout(s,250); setTimeout(s,900); setTimeout(s,2000);
})();
</script>
"@

$zip = [System.IO.Compression.ZipFile]::Open($ZipPath, 'Update')
try {
    $patched = 0
    foreach ($entry in @($zip.Entries)) {
        $name = $entry.FullName

        # 1) replace bundled logo images (png/svg/jpg/webp with logo-ish names)
        if ($name -match '(?i)(logo|brandmark|cfx[-_]?mark)' -and $name -match '(?i)\.(png|svg|jpg|jpeg|webp)$') {
            Write-Host ("logo asset -> NSM: " + $name)
            $entry.Delete()
            $newEntry = $zip.CreateEntry($name, [System.IO.Compression.CompressionLevel]::Optimal)
            $s = $newEntry.Open()
            $s.Write($logoBytes, 0, $logoBytes.Length)
            $s.Close()
            $patched++
            continue
        }

        if ($name -notmatch '(?i)\.(html|htm|json)$') { continue }
        $reader = New-Object System.IO.StreamReader($entry.Open(), [System.Text.Encoding]::UTF8)
        $content = $reader.ReadToEnd()
        $reader.Close()
        $original = $content

        # 2) HTML: NSM title + boot logo swap script
        if ($name -match '(?i)\.(html|htm)$') {
            $content = $content -replace '(?is)<title>[^<]*</title>', '<title>NSM</title>'
            if ($content -notmatch 'data-nsm="1"' -and $content -match '</body>') {
                $content = $content -replace '</body>', ($bootScript + '</body>')
            }
        }

        # 3) JSON manifests: display strings only
        if ($name -match '(?i)\.json$') {
            $content = $content -replace 'FiveM', 'NSM'
        }

        if ($content -ne $original) {
            $entry.Delete()
            $newEntry = $zip.CreateEntry($name, [System.IO.Compression.CompressionLevel]::Optimal)
            $writer = New-Object System.IO.StreamWriter($newEntry.Open(), (New-Object System.Text.UTF8Encoding($false)))
            $writer.Write($content)
            $writer.Close()
            $patched++
        }
    }
    Write-Host ("NSM logo patch complete: $patched entries updated in " + $ZipPath)
}
finally {
    $zip.Dispose()
}
