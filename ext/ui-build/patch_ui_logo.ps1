# NSM: swaps the boot/intro logo (the green->blue swipe that plays just before the
# server list) inside the prebuilt CfxUI bundle for the NSM "N" logo.
# Runs at build time (build.cmd) once per zip, AFTER the 7z step so every entry
# (bundle + loadscreen) is covered. Layered so the logo is replaced no matter how
# the app draws it: image entries, inline <svg>, CSS backgrounds, or canvas in
# intro containers.
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
$logoUrl = "data:image/png;base64,$logoB64"

# ---- boot-time DOM fixer: catches the animated/swipe intro however it mounts ----
$bootScript = @"
<script data-nsm="1">
(function(){
  var L='data:image/png;base64,$logoB64';
  var rx=/logo|wordmark|brandmark|cfx|five.?m|citizen|swipe|intro|splash/i;
  var nameRx=/logo|wordmark|brandmark|cfx|five.?m|citizen|swipe/i;
  function swapSvg(v){
    if(v.hasAttribute('data-nsm')) return;
    var n=document.createElement('img');
    n.src=L; n.setAttribute('data-nsm','1');
    if(v.getAttribute('class')) n.setAttribute('class', v.getAttribute('class')+' nsm-logo-swap');
    if(v.getAttribute('style')) n.setAttribute('style', v.getAttribute('style'));
    if(v.parentNode){ v.parentNode.replaceChild(n,v); }
  }
  function fix(){
    try{
      var els=document.querySelectorAll('img,svg,canvas');
      for(var i=0;i<els.length;i++){
        var el=els[i];
        if(el.hasAttribute('data-nsm')) continue;
        if(el.tagName==='IMG'){
          var src=el.getAttribute('src')||'', alt=el.getAttribute('alt')||'';
          if(nameRx.test(src)||nameRx.test(alt)){ el.src=L; el.setAttribute('data-nsm','1'); }
          else if(alt && /fivem|cfx|citizen/i.test(alt)){ el.alt='NSM'; }
        } else if(el.tagName==='SVG'){
          var key=(el.getAttribute('class')||'')+' '+(el.getAttribute('id')||'')+' '+(el.getAttribute('role')||'');
          var txt='';
          try{ txt=el.textContent||''; }catch(e){}
          if(nameRx.test(key)||/fivem|cfx|citizen/i.test(txt)){ swapSvg(el); }
        } else if(el.tagName==='CANVAS'){
          // canvas-drawn logo: only inside intro/splash/boot containers
          var p=el.closest?el.closest('[class*="intro" i],[class*="splash" i],[class*="boot" i],[class*="loading" i]'):null;
          if(p){ el.style.display='none'; if(!p.querySelector('img[data-nsm]')){ var n=document.createElement('img'); n.src=L; n.setAttribute('data-nsm','1'); p.appendChild(n); } }
        }
      }
      // intro/splash containers: force their first mark to NSM
      var cs=document.querySelectorAll('[class*="intro" i],[class*="splash" i]');
      for(var j=0;j<cs.length;j++){
        var c=cs[j];
        if(c.hasAttribute('data-nsm-fixed')) continue;
        var m=c.querySelector('img,svg,canvas');
        if(m){
          if(m.tagName==='CANVAS'){ m.style.display='none'; var n2=document.createElement('img'); n2.src=L; n2.setAttribute('data-nsm','1'); m.parentNode.insertBefore(n2,m); }
          else { m.src=L; m.setAttribute('data-nsm','1'); }
          c.setAttribute('data-nsm-fixed','1');
        }
      }
      // CSS background logos on logo-named elements
      var bg=document.querySelectorAll('[class*="logo" i],[id*="logo" i],[class*="wordmark" i],[id*="wordmark" i]');
      for(var k=0;k<bg.length;k++){
        var b=bg[k];
        if(b.tagName==='IMG'||b.tagName==='SVG') continue;
        var st=b.getAttribute('style')||'';
        if(/background-image/i.test(st)||rx.test(b.className||'')){ b.style.backgroundImage="url('"+L+"')"; b.style.backgroundSize='contain'; b.style.backgroundRepeat='no-repeat'; b.style.backgroundPosition='center'; }
      }
    }catch(e){}
  }
  fix();
  if(document.readyState!=='loading'){ fix(); } else { document.addEventListener('DOMContentLoaded', fix); }
  setTimeout(fix,250); setTimeout(fix,900); setTimeout(fix,2000);
  try{
    var mo=new MutationObserver(function(){ fix(); });
    var attach=function(){ if(document.body){ mo.observe(document.body,{childList:true,subtree:true,attributes:true,attributeFilter:['class','src','style']}); } };
    if(document.body) attach(); else document.addEventListener('DOMContentLoaded', attach);
  }catch(e){}
})();
</script>
<style data-nsm="1">img.nsm-logo-swap{object-fit:contain}</style>
"@

# ---- CSS layer: force any logo-named element's background to the NSM logo ----
$cssLayer = @"
/* NSM logo force */
[class*="logo" i],[id*="logo" i],[class*="wordmark" i],[id*="wordmark" i]{background-image:url('$logoUrl')!important;background-size:contain!important;background-repeat:no-repeat!important;background-position:center!important}
"@

# ---- image entry name net (skip our own nsm-* assets) ----
$nameNet = '(?i)(logo|wordmark|brandmark|branding|cfx|five.?m|citizen|swipe|intro|splash|loader|banner|hero|mark)'

$zip = [System.IO.Compression.ZipFile]::Open($ZipPath, 'Update')
try {
    $patched = 0
    foreach ($entry in @($zip.Entries)) {
        $name = $entry.FullName

        # never touch our own injected assets
        if ($name -match '(?i)(^|/)nsm-') { continue }

        # 1) replace bundled logo-ish images (png/svg/jpg/jpeg/webp/gif)
        if ($name -match $nameNet -and $name -match '(?i)\.(png|svg|jpg|jpeg|webp|gif)$') {
            Write-Host ("logo asset -> NSM: " + $name)
            $entry.Delete()
            $newEntry = $zip.CreateEntry($name, [System.IO.Compression.CompressionLevel]::Optimal)
            $s = $newEntry.Open()
            $s.Write($logoBytes, 0, $logoBytes.Length)
            $s.Close()
            $patched++
            continue
        }

        $ext = [System.IO.Path]::GetExtension($name).ToLowerInvariant()

        # 2) svg content sniff: FiveM/Cfx art smuggled in under a neutral name
        if ($ext -eq '.svg') {
            $reader = New-Object System.IO.StreamReader($entry.Open(), [System.Text.Encoding]::UTF8)
            $svgText = $reader.ReadToEnd()
            $reader.Close()
            if ($svgText -match '(?i)(fivem|cfx|citizen)') {
                Write-Host ("svg content -> NSM: " + $name)
                $entry.Delete()
                $newEntry = $zip.CreateEntry($name, [System.IO.Compression.CompressionLevel]::Optimal)
                $s = $newEntry.Open()
                $s.Write($logoBytes, 0, $logoBytes.Length)
                $s.Close()
                $patched++
            }
            continue
        }

        if ($ext -notin @('.html', '.htm', '.json', '.css')) { continue }
        $reader = New-Object System.IO.StreamReader($entry.Open(), [System.Text.Encoding]::UTF8)
        $content = $reader.ReadToEnd()
        $reader.Close()
        $original = $content

        if ($ext -in @('.html', '.htm')) {
            if ($content -notmatch 'data-nsm="1"') {
                $content = $content -replace '(?is)<title>[^<]*</title>', '<title>NSM</title>'
                $content = $content -replace '(?i)alt="[^"]*(fivem|cfx|citizen)[^"]*"', 'alt="NSM"'
                if ($content -match '</head>') {
                    $content = $content -replace '</head>', ('<style data-nsm="1">' + $cssLayer + '</style>' + '</head>')
                }
                if ($content -match '</body>') {
                    $content = $content -replace '</body>', ($bootScript + '</body>')
                }
            }
        }
        if ($ext -eq '.css') {
            $content = $content + "`n" + $cssLayer
        }
        if ($ext -eq '.json') {
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

    # 3) ensure our logo is present in the bundle for anything that wants it
    $hasLogo = $false
    foreach ($entry in $zip.Entries) { if ($entry.FullName -match '(?i)(^|/)nsm-logo\.png$') { $hasLogo = $true; break } }
    if (!$hasLogo) {
        $newEntry = $zip.CreateEntry('nsm-logo.png', [System.IO.Compression.CompressionLevel]::Optimal)
        $s = $newEntry.Open()
        $s.Write($logoBytes, 0, $logoBytes.Length)
        $s.Close()
        $patched++
    }

    Write-Host ("NSM logo patch complete: $patched entries updated in " + $ZipPath)
}
finally {
    $zip.Dispose()
}
