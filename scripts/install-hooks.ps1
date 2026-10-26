param(
    [string]$GitleaksVersion = "8.30.1"
)

$ErrorActionPreference = "Stop"
$repoRoot = (git rev-parse --show-toplevel).Trim()
if (-not $repoRoot) {
    throw "Run this script inside the freedeep repository."
}

git -C $repoRoot config core.hooksPath .githooks

$existing = Get-Command gitleaks -ErrorAction SilentlyContinue
if ($existing) {
    $gitleaksPath = $existing.Source
} else {
    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
        throw "GitHub CLI is required to download Gitleaks. Install gh or place gitleaks on PATH."
    }
    $architecture = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
        "X64" { "x64" }
        "Arm64" { "arm64" }
        default { throw "Unsupported Windows architecture: $_" }
    }
    $toolDir = Join-Path $repoRoot ".git/tools/gitleaks-$GitleaksVersion"
    New-Item -ItemType Directory -Force -Path $toolDir | Out-Null
    $archive = "gitleaks_${GitleaksVersion}_windows_${architecture}.zip"
    gh release download "v$GitleaksVersion" --repo gitleaks/gitleaks --pattern $archive --dir $toolDir --clobber
    Expand-Archive -LiteralPath (Join-Path $toolDir $archive) -DestinationPath $toolDir -Force
    $gitleaksPath = Join-Path $toolDir "gitleaks.exe"
}

$hookPath = $gitleaksPath.Replace("\", "/")
git -C $repoRoot config freedeep.gitleaksPath $hookPath

Write-Host "freedeep hooks installed."
Write-Host "Hooks path: .githooks"
Write-Host "Gitleaks: $gitleaksPath"
Write-Host "Every commit and push will now be checked automatically."
