<#
.SYNOPSIS
    Windows equivalent of the Makefile. The Makefile stays canonical for CI and
    Unix contributors; this exists so the same targets work in PowerShell
    without installing make.

.EXAMPLE
    .\make.ps1 ci
#>
[CmdletBinding()]
param(
    [ValidateSet('help', 'build', 'test', 'race', 'cover', 'fmt', 'vet', 'lint', 'tidy', 'vuln', 'fuzz', 'ci', 'clean')]
    [string]$Target = 'help'
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

function Invoke-Step {
    param([string]$Name, [scriptblock]$Body)
    Write-Host "==> $Name" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) { throw "$Name failed with exit code $LASTEXITCODE" }
}

function Get-BuildMetadata {
    # Must work outside a git checkout too, e.g. a source tarball.
    $version = 'dev'
    $commit = 'none'
    try {
        $v = & git describe --tags --always --dirty 2>$null
        if ($LASTEXITCODE -eq 0 -and $v) { $version = "$v".Trim() }
        $c = & git rev-parse --short HEAD 2>$null
        if ($LASTEXITCODE -eq 0 -and $c) { $commit = "$c".Trim() }
    }
    catch {
        # No git, or not a repository; the defaults above are correct.
    }
    $global:LASTEXITCODE = 0

    [pscustomobject]@{
        Version = $version
        Commit  = $commit
        Date    = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    }
}

function Target-Build {
    $m = Get-BuildMetadata
    $ldflags = "-s -w -X main.version=$($m.Version) -X main.commit=$($m.Commit) -X main.date=$($m.Date)"
    Invoke-Step 'build' { go build -trimpath -ldflags $ldflags -o bin/shutdowncheck.exe ./cmd/shutdowncheck }
}

function Target-Fmt {
    Write-Host '==> fmt' -ForegroundColor Cyan
    $unformatted = (gofmt -l .) | Where-Object { $_ -and ($_ -notlike 'test\conformance*') }
    if ($unformatted) {
        Write-Host "not gofmt'd:" -ForegroundColor Red
        $unformatted | ForEach-Object { Write-Host "  $_" }
        throw 'formatting check failed; run: gofmt -w .'
    }
}

function Target-Lint {
    Target-Fmt
    Invoke-Step 'vet' { go vet ./... }
    if (Get-Command golangci-lint -ErrorAction SilentlyContinue) {
        Invoke-Step 'golangci-lint' { golangci-lint run }
    }
    else {
        Write-Host 'golangci-lint not installed, skipping (see CONTRIBUTING.md)' -ForegroundColor Yellow
    }
}

function Target-Vuln {
    if (Get-Command govulncheck -ErrorAction SilentlyContinue) {
        Invoke-Step 'govulncheck' { govulncheck ./... }
    }
    else {
        Write-Host 'govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@latest' -ForegroundColor Yellow
    }
}

# The race detector needs cgo and a C toolchain, which a bare Windows install
# usually lacks. CI covers race on Linux and macOS, so skip rather than fail.
function Target-Race {
    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
        Write-Host 'race detector needs cgo and a C compiler (gcc); skipping locally. CI runs it on Linux and macOS.' -ForegroundColor Yellow
        return
    }
    Invoke-Step 'race' { go test -race ./... }
}

function Target-Cover {
    # Quoting matters: PowerShell 5.1 splits bare -flag=value.ext arguments.
    Invoke-Step 'cover' { go test '-covermode=atomic' '-coverprofile=coverage.out' ./... }
    go tool cover '-func=coverage.out' | Select-Object -Last 1
}

switch ($Target) {
    'help' {
        Write-Host 'Targets: build test race cover fmt vet lint tidy vuln fuzz ci clean'
        Write-Host 'Usage:   .\make.ps1 ci'
    }
    'build' { Target-Build }
    'test' { Invoke-Step 'test' { go test ./... } }
    'race' { Target-Race }
    'cover' { Target-Cover }
    'fmt' { Target-Fmt }
    'vet' { Invoke-Step 'vet' { go vet ./... } }
    'lint' { Target-Lint }
    'tidy' {
        Invoke-Step 'tidy' { go mod tidy }
        git diff --quiet -- go.mod go.sum
        if ($LASTEXITCODE -ne 0) { throw "go.mod/go.sum are not tidy; commit the result of 'go mod tidy'" }
    }
    'vuln' { Target-Vuln }
    'fuzz' { Write-Host 'no fuzz targets yet; config and NDJSON parsers get them in Phase 1' }
    'ci' {
        Target-Lint
        Invoke-Step 'test' { go test ./... }
        Target-Race
        Target-Cover
        Write-Host 'CI checks passed' -ForegroundColor Green
    }
    'clean' {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue bin, coverage.out
        Write-Host 'cleaned'
    }
}
