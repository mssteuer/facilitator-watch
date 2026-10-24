#!/bin/sh
# Source/history only; never read environment files or keys, never print matches.
set -eu
pattern='-----BEGIN ([A-Z0-9]+ )?PRIVATE KEY-----|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[A-Z0-9]{16}|sk-[A-Za-z0-9]{32,}|xox[baprs]-[A-Za-z0-9-]{20,}'
found=0
for file in ./*.go ./*.md ./dev.sh ./web/*.html ./web/*.css ./web/*.js; do
  [ -f "$file" ] || continue
  if grep -E -q -- "$pattern" "$file"; then
    printf 'Potential secret in source: %s (contents suppressed)\n' "$file"
    found=1
  fi
done
# Scan only project source paths in every local revision, not arbitrary blobs.
for revision in $(git rev-list --all); do
  if git grep -I -q -E -e "$pattern" "$revision" -- '*.go' '*.md' dev.sh 'web/*.html' 'web/*.css' 'web/*.js'; then
    printf 'Potential secret in history revision: %s (contents suppressed)\n' "$revision"
    found=1
  fi
done
if [ "$found" -ne 0 ]; then exit 1; fi
printf 'Source and local history heuristic secret scan: no matches.\n'
