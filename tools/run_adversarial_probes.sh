#!/usr/bin/env bash
# Generate adversarial PDFs and run the probe matrix + go tests.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

SCALE="${ADVERSARIAL_PDF_SCALE:-medium}"
OUT="${ADVERSARIAL_PDF_DIR:-$ROOT/tools/adversarial_pdfs}"

echo "==> Generating scale=${SCALE} fixtures into ${OUT}"
python3 tools/gen_adversarial_pdfs.py -o "$OUT" --scale "$SCALE"

echo
echo "==> Probe matrix (timeout variants)"
printf '%-22s %-10s %-12s %-14s %s\n' "FIXTURE" "TIMEOUT" "ELAPSED" "TOTAL_ALLOC" "ERR"
printf '%-22s %-10s %-12s %-14s %s\n' "-------" "-------" "-------" "-----------" "---"

probe() {
  local file="$1" timeout="$2"
  local out
  out="$(go run ./tools/probe_extract.go -timeout "$timeout" "$file" 2>&1)"
  local elapsed alloc err
  elapsed="$(echo "$out" | sed -n 's/.*elapsed=\([^ ]*\).*/\1/p' | head -1)"
  alloc="$(echo "$out" | sed -n 's/.*total_alloc_delta=\([0-9]*\).*/\1/p' | head -1)"
  err="$(echo "$out" | sed -n 's/.*extract_err=\([^ ]*\).*/\1/p' | head -1)"
  local base
  base="$(basename "$file" ".${SCALE}.pdf")"
  printf '%-22s %-10s %-12s %-14s %s\n' "$base" "$timeout" "${elapsed:-?}" "${alloc:-?}" "${err:-?}"
}

for f in "$OUT"/*."${SCALE}".pdf; do
  probe "$f" 200µs
done
echo
for f in "$OUT"/*."${SCALE}".pdf; do
  # No deadline: measure full-path cost
  probe "$f" 0
done

echo
echo "==> go test -tags=adversarial (desired behavior; failures = real gaps)"
set +e
ADVERSARIAL_PDF_DIR="$OUT" ADVERSARIAL_PDF_SCALE="$SCALE" \
  go test -tags=adversarial -count=1 -v -run Adversarial .
test_rc=$?
set -e
exit "$test_rc"
